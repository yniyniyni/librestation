package daemon

import (
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"voice/internal/kws"
)

// constHead scores every window sigmoid(bias).
func constHead(bias float32) *kws.Head {
	return &kws.Head{
		W1: make([]float32, kws.EmbDim*kws.H1), B1: make([]float32, kws.H1),
		W2: make([]float32, kws.Ctx*kws.H1*kws.H2), B2: make([]float32, kws.H2),
		W3: make([]float32, kws.H2), B3: []float32{bias},
	}
}

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

// music returns a block of loud playback reference (int16 scale).
func music(n int) []float32 {
	b := make([]float32, n)
	for i := range b {
		b[i] = float32(8000 * math.Sin(float64(i)*0.3))
	}
	return b
}

func withWord(d *Daemon, bias float32) {
	d.mu.Lock()
	d.words = []word{{ID: "x", Model: &kws.Model{Name: "x", Threshold: 0.5, Head: constHead(bias)}}}
	d.mu.Unlock()
}

func feedRef(d *Daemon, secs float64, blk func(int) []float32) {
	for range int(secs * 50) {
		d.refw.push(blk(320))
	}
}

func TestWordInPlaybackBlocksAction(t *testing.T) {
	f := &fakeAct{}
	d, _ := newTest(t, f, "pause", false)
	withWord(d, 5) // the playback scores 0.99 >= 0.8*0.5
	feedRef(d, 3, music)
	hit(d)
	time.Sleep(50 * time.Millisecond)
	h := d.state().Hits[0]
	if len(f.got()) != 0 || h.Fired || !h.Blocked || h.RefScore < 0.9 || h.Verdict != "false" || !h.Auto {
		t.Fatalf("calls %v hit %+v", f.got(), h)
	}
	if _, ok := d.lastAction["pause"]; ok {
		t.Fatal("a blocked hit must not debounce the next real command")
	}
	d.waitIO()
	if _, err := os.Stat(filepath.Join(d.wordDir("x"), h.FalseFile)); err != nil {
		t.Fatal("false clip not saved:", err)
	}
}

func TestWordNotInPlaybackFires(t *testing.T) {
	f := &fakeAct{}
	d, done := newTest(t, f, "pause", false)
	withWord(d, -5)
	feedRef(d, 3, music)
	hit(d)
	<-done
	h := d.state().Hits[0]
	if len(f.got()) != 1 || h.Blocked || h.RefScore < 0 || h.RefScore > 0.1 {
		t.Fatalf("calls %v hit %+v", f.got(), h)
	}
}

func TestSilentPlaybackIsNotChecked(t *testing.T) {
	f := &fakeAct{}
	d, done := newTest(t, f, "pause", false)
	withWord(d, 5)
	feedRef(d, 3, music)
	feedRef(d, 1, func(n int) []float32 { return make([]float32, n) }) // paused
	hit(d)
	<-done
	if h := d.state().Hits[0]; len(f.got()) != 1 || h.RefScore != -1 {
		t.Fatalf("calls %v hit %+v", f.got(), h)
	}

	d.settings.RefCheck = false
	feedRef(d, 3, music)
	d.lastAction = map[string]time.Time{}
	hit(d)
	<-done
	if h := d.state().Hits[0]; len(f.got()) != 2 || h.RefScore != -1 {
		t.Fatalf("check off: calls %v hit %+v", f.got(), h)
	}
}
