package daemon

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeAssistant struct {
	mu     sync.Mutex
	busy   bool
	wakes  int
	blocks int
	noReq  func()
}

func (f *fakeAssistant) Audio([]int16) { f.mu.Lock(); f.blocks++; f.mu.Unlock() }
func (f *fakeAssistant) Wake(_ context.Context, n func()) {
	f.mu.Lock()
	f.wakes++
	f.noReq = n
	f.mu.Unlock()
}
func (f *fakeAssistant) Busy() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.busy }
func (f *fakeAssistant) setBusy(b bool) {
	f.mu.Lock()
	f.busy = b
	f.mu.Unlock()
}

func newAsst(t *testing.T, act *fakeAct, action string) (*Daemon, *fakeAssistant) {
	t.Helper()
	d, _ := newTest(t, act, action, false)
	a := &fakeAssistant{}
	d.opt.Assistant = a
	return d, a
}

func TestAssistantActionWakes(t *testing.T) {
	f := &fakeAct{}
	d, a := newAsst(t, f, "assistant")
	hit(d)
	time.Sleep(50 * time.Millisecond)
	if a.wakes != 1 || len(f.got()) != 0 {
		t.Fatalf("wakes %d, actuator %v", a.wakes, f.got())
	}
	if h := d.state().Hits[0]; !h.Fired || h.Debounced {
		t.Fatalf("hit %+v", h)
	}
	hit(d) // not debounced against itself
	if a.wakes != 2 {
		t.Fatalf("wakes %d", a.wakes)
	}
}

func TestAssistantWakesWhileBusy(t *testing.T) {
	d, a := newAsst(t, &fakeAct{}, "assistant")
	a.setBusy(true)
	hit(d)
	if a.wakes != 1 {
		t.Fatalf("barge-in: wakes %d", a.wakes)
	}
}

func TestAssistantHitMarkedFalse(t *testing.T) {
	d, a := newAsst(t, &fakeAct{}, "assistant")
	hit(d)
	a.noReq()
	d.waitIO()
	h := d.state().Hits[0]
	if h.Verdict != "false" || !h.Auto || h.FalseFile == "" {
		t.Fatalf("hit %+v", h)
	}
	if n, _ := filepath.Glob(filepath.Join(d.wordDir("x"), "false-*.wav")); len(n) == 0 {
		t.Fatal("false clip not saved")
	}
}

func TestAssistantAudioEveryBlock(t *testing.T) {
	d, a := newAsst(t, &fakeAct{}, "")
	d.onAudio(make([]int16, 320))
	d.onAudio(make([]int16, 320))
	if a.blocks != 2 {
		t.Fatalf("blocks %d", a.blocks)
	}
}

func TestAssistantDryRunOnlyLogs(t *testing.T) {
	d, a := newAsst(t, &fakeAct{}, "assistant")
	d.settings.DryRun = true
	hit(d)
	if a.wakes != 0 || len(d.state().Hits) != 1 {
		t.Fatalf("wakes %d", a.wakes)
	}
}

func TestQuickCommandsSuppressedWhileBusy(t *testing.T) {
	f := &fakeAct{}
	d, a := newAsst(t, f, "pause")
	a.setBusy(true)
	hit(d)
	time.Sleep(50 * time.Millisecond)
	h := d.state().Hits[0]
	if len(f.got()) != 0 || h.Fired || !h.Debounced {
		t.Fatalf("calls %v hit %+v", f.got(), h)
	}
}

func TestQuickCommandsWhileAssistantErrored(t *testing.T) {
	f := &fakeAct{}
	d, a := newAsst(t, f, "pause")
	a.setBusy(false)
	hit(d)
	deadline := time.Now().Add(2 * time.Second)
	for len(f.got()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if g := f.got(); len(g) != 1 || g[0] != "pause" {
		t.Fatalf("calls %v", g)
	}
}

func TestAssistantActionValid(t *testing.T) {
	d, _ := newAsst(t, &fakeAct{}, "")
	srv := httptest.NewServer(d.Handler())
	defer srv.Close()
	if r := post(t, srv.URL+"/api/words/x", `{"action":"assistant"}`); r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
}
