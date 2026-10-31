package daemon

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"voice/internal/judge"
)

type fakeJudge struct {
	mu   sync.Mutex
	v    judge.Verdict
	err  error
	seen []judge.Hit
}

func (f *fakeJudge) Judge(_ context.Context, h judge.Hit) (judge.Verdict, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, h)
	return f.v, f.err
}

func (f *fakeJudge) calls() []judge.Hit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]judge.Hit(nil), f.seen...)
}

// judgeTest is a daemon whose hits go to j right away.
func judgeTest(t *testing.T, j *fakeJudge, action string) (*Daemon, chan struct{}) {
	t.Helper()
	d, done := newTest(t, &fakeAct{}, action, false)
	d.opt.Judge = j
	d.judgeDelay = 0
	d.ring.write(make([]int16, 16000)) // a second of mic for the clip
	ctx, cancel := context.WithCancel(context.Background())
	go d.runJudge(ctx)
	t.Cleanup(cancel)
	return d, done
}

// waitJudged waits until the newest hit has a call or an error.
func waitJudged(t *testing.T, d *Daemon) Hit {
	t.Helper()
	for range 400 {
		if h := d.state().Hits[0]; h.Judge != nil || h.JudgeError != "" {
			d.waitIO()
			return d.state().Hits[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("hit was not judged")
	return Hit{}
}

func falseClips(d *Daemon) int {
	n, _ := filepath.Glob(filepath.Join(d.wordDir("x"), "false-*.wav"))
	return len(n)
}

func TestJudgedFalseAlarmSavesClip(t *testing.T) {
	j := &fakeJudge{v: judge.Verdict{Command: false, Confidence: 0.9, Heard: "стоп машина", Reason: "песня"}}
	d, done := judgeTest(t, j, "pause")
	d.ObservePlayback("playing")
	hit(d)
	<-done
	h := waitJudged(t, d)
	if h.Verdict != "false" || !h.Auto || h.Judge.Heard != "стоп машина" || falseClips(d) != 1 {
		t.Fatalf("hit %+v, %d clips", h, falseClips(d))
	}
	c := j.calls()
	if len(c) != 1 || c[0].Word != "x" || c[0].Action != "pause" || !c[0].Playback || len(c[0].Audio) != 16000 {
		t.Fatalf("judge got %+v", c)
	}
}

func TestJudgedCommandIsOK(t *testing.T) {
	d, done := judgeTest(t, &fakeJudge{v: judge.Verdict{Command: true, Confidence: 0.95}}, "pause")
	hit(d)
	<-done
	if h := waitJudged(t, d); h.Verdict != "ok" || !h.Auto || falseClips(d) != 0 {
		t.Fatalf("hit %+v", h)
	}
}

func TestUnsureCallLeavesVerdictOpen(t *testing.T) {
	d, done := judgeTest(t, &fakeJudge{v: judge.Verdict{Command: false, Confidence: 0.4}}, "pause")
	hit(d)
	<-done
	if h := waitJudged(t, d); h.Verdict != "" || h.Judge == nil || falseClips(d) != 0 {
		t.Fatalf("hit %+v", h)
	}
}

func TestJudgeKeepsUserVerdict(t *testing.T) {
	j := &fakeJudge{v: judge.Verdict{Command: false, Confidence: 0.99}}
	d, done := judgeTest(t, j, "pause")
	d.judgeDelay = time.Hour // judged only when asked below
	hit(d)
	<-done
	srv := httptest.NewServer(d.Handler())
	defer srv.Close()
	post(t, srv.URL+"/api/hits/1/ok", "")
	if r := post(t, srv.URL+"/api/hits/1/judge", ""); r.StatusCode != 200 {
		t.Fatalf("rejudge: %d", r.StatusCode)
	}
	if h := waitJudged(t, d); h.Verdict != "ok" || h.Auto || falseClips(d) != 0 {
		t.Fatalf("model overrode the user: %+v", h)
	}
}

// A take-back marked the hit false; the model heard a real command, so the
// station's own guess is replaced and the clip taken back.
func TestJudgeOverridesTakeBack(t *testing.T) {
	j := &fakeJudge{v: judge.Verdict{Command: true, Confidence: 0.9}}
	d, done := judgeTest(t, j, "pause")
	d.judgeDelay = 50 * time.Millisecond
	d.settings.AutoFalseUndo = true
	clock := time.Now()
	d.now = func() time.Time { return clock }
	d.ObservePlayback("playing")
	hit(d)
	<-done
	d.ObservePlayback("paused")
	clock = clock.Add(2 * time.Second)
	d.ObservePlayback("playing") // taken back in the app
	d.waitIO()
	if h := d.state().Hits[0]; h.Verdict != "false" || falseClips(d) != 1 {
		t.Fatalf("take-back not marked: %+v", h)
	}
	h := waitJudged(t, d)
	if h.Verdict != "ok" || falseClips(d) != 0 {
		t.Fatalf("hit %+v, %d clips", h, falseClips(d))
	}
	if c := j.calls(); len(c) != 1 || c[0].Undone != 2 {
		t.Fatalf("judge got %+v", c)
	}
}

func TestJudgeErrorShown(t *testing.T) {
	d, done := judgeTest(t, &fakeJudge{err: errors.New("gemini: FAILED_PRECONDITION: User location is not supported")}, "pause")
	hit(d)
	<-done
	if h := waitJudged(t, d); h.JudgeError == "" || h.Verdict != "" {
		t.Fatalf("hit %+v", h)
	}
}

func TestJudgeOffOrMissing(t *testing.T) {
	j := &fakeJudge{v: judge.Verdict{Command: true, Confidence: 1}}
	d, done := judgeTest(t, j, "pause")
	d.settings.JudgeOff = true
	hit(d)
	<-done
	time.Sleep(30 * time.Millisecond)
	if len(j.calls()) != 0 || !d.state().Judge.Available || d.state().Judge.On {
		t.Fatalf("judged with the switch off: %+v", d.state().Judge)
	}

	d2, done2 := newTest(t, &fakeAct{}, "pause", false)
	hit(d2)
	<-done2
	srv := httptest.NewServer(d2.Handler())
	defer srv.Close()
	if r := post(t, srv.URL+"/api/hits/1/judge", ""); r.StatusCode == 200 || d2.state().Judge.Available {
		t.Fatalf("rejudge without a model: %d", r.StatusCode)
	}
}

func TestJudgedHitSurvivesRestart(t *testing.T) {
	j := &fakeJudge{v: judge.Verdict{Command: false, Confidence: 0.8, Reason: "ТВ"}}
	d, done := judgeTest(t, j, "pause")
	hit(d)
	<-done
	waitJudged(t, d)
	d2, err := New(Options{DataDir: d.opt.DataDir})
	if err != nil {
		t.Fatal(err)
	}
	if h := d2.state().Hits[0]; h.Judge == nil || h.Judge.Reason != "ТВ" || h.Verdict != "false" {
		t.Fatalf("reloaded %+v", h)
	}
	if _, err := os.Stat(filepath.Join(d.wordDir("x"), d2.state().Hits[0].FalseFile)); err != nil {
		t.Fatal(err)
	}
}
