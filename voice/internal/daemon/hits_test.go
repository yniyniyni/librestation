package daemon

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// clockTest is newTest with a settable clock.
func clockTest(t *testing.T, action string) (*Daemon, *time.Time, chan struct{}) {
	d, done := newTest(t, &fakeAct{}, action, false)
	clock := time.Unix(1000, 0)
	d.now = func() time.Time { return clock }
	return d, &clock, done
}

func fireWord(d *Daemon, id string) {
	d.mu.Lock()
	d.onHit(id, 0.9)
	d.mu.Unlock()
}

func TestHitsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	d, err := New(Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	d.settings.Words["x"] = &WordSettings{Enabled: true}
	for range maxHits + 5 {
		hit(d)
	}
	d.waitIO()
	if n, _ := filepath.Glob(filepath.Join(dir, "hits", "*.json")); len(n) != maxHits {
		t.Fatalf("%d hit files on disk, want %d", len(n), maxHits)
	}

	d2, err := New(Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d2.waitIO)
	hs := d2.state().Hits
	if len(hs) != maxHits || hs[0].ID != maxHits+5 || hs[maxHits-1].ID != 6 {
		t.Fatalf("reloaded %d hits, newest %d", len(hs), hs[0].ID)
	}
	srv := httptest.NewServer(d2.Handler())
	defer srv.Close()
	r, err := http.Get(srv.URL + "/api/hits/30")
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("audio of a reloaded hit: %v %v", err, r.Status)
	}
	r.Body.Close()
	hit(d2)
	if id := d2.state().Hits[0].ID; id != maxHits+6 {
		t.Fatalf("new hit id %d continues no sequence", id)
	}
}

func TestVerdictSavesAndTakesBackClip(t *testing.T) {
	d, _ := newTest(t, nil, "", false)
	hit(d)
	srv := httptest.NewServer(d.Handler())
	defer srv.Close()
	falses := func() int {
		n, _ := filepath.Glob(filepath.Join(d.wordDir("x"), "false-*.wav"))
		return len(n)
	}
	if r := post(t, srv.URL+"/api/hits/1/false", ""); r.StatusCode != 200 || falses() != 1 {
		t.Fatalf("false: status %d, %d clips", r.StatusCode, falses())
	}
	if h := d.state().Hits[0]; h.Verdict != "false" || h.Auto {
		t.Fatalf("hit %+v", h)
	}
	if r := post(t, srv.URL+"/api/hits/1/ok", ""); r.StatusCode != 200 || falses() != 0 {
		t.Fatalf("ok: status %d, %d clips", r.StatusCode, falses())
	}
	if r := post(t, srv.URL+"/api/hits/1/maybe", ""); r.StatusCode != 400 {
		t.Fatalf("unknown verdict: status %d", r.StatusCode)
	}
}

func TestPauseTakenBackFromApp(t *testing.T) {
	d, clock, done := clockTest(t, "pause")
	d.ObservePlayback("playing")
	fireWord(d, "x")
	<-done
	*clock = clock.Add(200 * time.Millisecond)
	d.ObservePlayback("paused")  // our own pause
	d.ObservePlayback("playing") // stale report inside the guard
	if h := d.state().Hits[0]; h.Undone != 0 {
		t.Fatalf("flagged inside the guard: %+v", h)
	}
	d.ObservePlayback("paused")
	*clock = clock.Add(3 * time.Second)
	d.ObservePlayback("playing") // resumed in the Spotify app
	if h := d.state().Hits[0]; h.Undone < 3 || h.Verdict != "" {
		t.Fatalf("pause taken back: %+v", h)
	}
}

func TestTakenBackTooLateIsNotFlagged(t *testing.T) {
	d, clock, done := clockTest(t, "pause")
	d.ObservePlayback("playing")
	fireWord(d, "x")
	<-done
	d.ObservePlayback("paused")
	*clock = clock.Add(undoWindow + time.Second)
	d.ObservePlayback("playing")
	if h := d.state().Hits[0]; h.Undone != 0 {
		t.Fatalf("flagged after the window: %+v", h)
	}
}

func TestNextTakenBackByVoiceAutoMarked(t *testing.T) {
	d, clock, done := clockTest(t, "next")
	d.settings.AutoFalseUndo = true
	d.settings.Words["back"] = &WordSettings{Enabled: true, Action: "prev"}
	fireWord(d, "x")
	<-done
	*clock = clock.Add(2 * time.Second)
	fireWord(d, "back")
	<-done
	hs := d.state().Hits
	if hs[0].Undone != 0 || hs[1].Undone != 2 || hs[1].Verdict != "false" || !hs[1].Auto {
		t.Fatalf("hits %+v", hs)
	}
	d.waitIO()
	if _, err := os.Stat(filepath.Join(d.wordDir("x"), hs[1].FalseFile)); err != nil {
		t.Fatal("auto false clip not saved:", err)
	}
}

func TestOnlyLatestCommandIsTakenBack(t *testing.T) {
	d, clock, done := clockTest(t, "next")
	d.settings.Words["p"] = &WordSettings{Enabled: true, Action: "pause"}
	d.settings.Words["back"] = &WordSettings{Enabled: true, Action: "prev"}
	fireWord(d, "x")
	<-done
	*clock = clock.Add(2 * time.Second)
	fireWord(d, "p")
	<-done
	*clock = clock.Add(2 * time.Second)
	fireWord(d, "back")
	<-done
	for _, h := range d.state().Hits {
		if h.Undone != 0 {
			t.Fatalf("flagged %+v", h)
		}
	}
}

func TestLouderTakenBackWithKnob(t *testing.T) {
	d, clock, done := clockTest(t, "louder")
	d.ObserveVolume(15)
	fireWord(d, "x")
	<-done
	d.ObserveVolume(20) // our step
	*clock = clock.Add(4 * time.Second)
	d.ObserveVolume(18) // knob fine-tuning: not a take-back
	if h := d.state().Hits[0]; h.Undone != 0 {
		t.Fatalf("flagged a fine-tune: %+v", h)
	}
	d.ObserveVolume(15)
	if h := d.state().Hits[0]; h.Undone != 4 {
		t.Fatalf("louder taken back: %+v", h)
	}
}

func TestLouderAtMaxIsNotFlagged(t *testing.T) {
	d, clock, done := clockTest(t, "louder")
	d.ObserveVolume(100)
	fireWord(d, "x")
	<-done
	*clock = clock.Add(2 * time.Second)
	d.ObserveVolume(98)
	if h := d.state().Hits[0]; h.Undone != 0 {
		t.Fatalf("flagged a no-op louder: %+v", h)
	}
}
