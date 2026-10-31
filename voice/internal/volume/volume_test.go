package volume

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"voice/internal/input"
	"voice/internal/soloist"
)

type fakeSetter struct {
	mu   sync.Mutex
	sets []int
	err  error
}

func (f *fakeSetter) SetVolume(v int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sets = append(f.sets, v)
	return nil
}

func (f *fakeSetter) got() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.sets...)
}

func vol(v int) soloist.Event { return soloist.Event{Type: "volume_changed", Volume: &v} }

func newSync(t *testing.T, saved string) (*Sync, *fakeSetter, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "volume")
	if saved != "" {
		if err := os.WriteFile(path, []byte(saved), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeSetter{}
	s := New(f, path)
	if err := s.SetKnobStep(5); err != nil { // the tests below count in 5s
		t.Fatal(err)
	}
	if err := s.Apply(); err != nil {
		t.Fatal(err)
	}
	return s, f, path
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func saved(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func TestDefaultVolume15(t *testing.T) {
	s, f, _ := newSync(t, "")
	if got := f.got(); !equal(got, []int{15}) {
		t.Fatalf("sets at start = %v, want [15]", got)
	}
	// Applying again (e.g. a second Run) must not set anything new.
	s.Apply()
	if got := f.got(); !equal(got, []int{15}) {
		t.Fatalf("sets after second Apply = %v, want [15]", got)
	}
	// Soloist starts at its own --initial-volume; an early report of a higher
	// value must not raise the volume either.
	s.OnEvent(vol(40))
	if v := s.Volume(); v > 15 && len(f.got()) > 1 {
		t.Fatalf("volume raised on start: %d, sets %v", v, f.got())
	}
}

func TestSavedVolumeRestored(t *testing.T) {
	_, f, _ := newSync(t, "35\n")
	if got := f.got(); !equal(got, []int{35}) {
		t.Fatalf("sets at start = %v, want [35]", got)
	}
}

func TestBadSavedVolumeFallsBackTo15(t *testing.T) {
	_, f, _ := newSync(t, "loud")
	if got := f.got(); !equal(got, []int{15}) {
		t.Fatalf("sets at start = %v, want [15]", got)
	}
}

func TestKnobStepsBy5(t *testing.T) {
	s, f, path := newSync(t, "15")
	s.Key(input.KeyPageUp)
	s.Key(input.KeyPageUp)
	s.Key(input.KeyPageDown)
	if got := f.got(); !equal(got, []int{15, 20, 25, 20}) {
		t.Fatalf("sets = %v, want [15 20 25 20]", got)
	}
	if got := saved(t, path); got != "20" {
		t.Fatalf("saved %q, want 20", got)
	}

	hi, fh, _ := newSync(t, "98")
	hi.Key(input.KeyPageUp)
	hi.Key(input.KeyPageUp) // already 100: no extra set
	if got := fh.got(); !equal(got, []int{98, 100}) {
		t.Fatalf("top clamp sets = %v, want [98 100]", got)
	}
	lo, fl, _ := newSync(t, "3")
	lo.Key(input.KeyPageDown)
	lo.Key(input.KeyPageDown)
	if got := fl.got(); !equal(got, []int{3, 0}) {
		t.Fatalf("bottom clamp sets = %v, want [3 0]", got)
	}
	lo.Key(30) // some other key: ignored
	if got := fl.got(); !equal(got, []int{3, 0}) {
		t.Fatalf("other key changed volume: %v", got)
	}
}

func TestStepImplementsActuatorStep(t *testing.T) {
	s, f, _ := newSync(t, "50")
	if err := s.Step(5); err != nil {
		t.Fatal(err)
	}
	if err := s.Step(-10); err != nil {
		t.Fatal(err)
	}
	if got := f.got(); !equal(got, []int{50, 55, 45}) {
		t.Fatalf("sets = %v", got)
	}
}

func TestSyncNoEcho(t *testing.T) {
	s, f, _ := newSync(t, "15")
	s.Key(input.KeyPageUp) // -> 20
	if got := f.got(); !equal(got, []int{15, 20}) {
		t.Fatalf("sets after knob = %v, want [15 20]", got)
	}
	s.OnEvent(vol(15)) // echo of the start value, late
	s.OnEvent(vol(20)) // echo of the knob
	s.OnEvent(soloist.Event{Type: "playback_state", Volume: ptr(20)})
	if got := f.got(); !equal(got, []int{15, 20}) {
		t.Fatalf("echo caused sets: %v", got)
	}
	if s.Volume() != 20 {
		t.Fatalf("volume = %d, want 20", s.Volume())
	}
}

func TestFastKnobStaleEchoIgnored(t *testing.T) {
	s, f, _ := newSync(t, "15")
	s.Key(input.KeyPageUp) // 20
	s.Key(input.KeyPageUp) // 25
	s.OnEvent(vol(20))     // echo of the first turn arrives after the second
	if s.Volume() != 25 {
		t.Fatalf("stale echo moved volume to %d", s.Volume())
	}
	s.Key(input.KeyPageUp) // must go 25 -> 30
	if got := f.got(); !equal(got, []int{15, 20, 25, 30}) {
		t.Fatalf("sets = %v", got)
	}
}

func TestSliderChangeAdoptedAndSaved(t *testing.T) {
	s, f, path := newSync(t, "15")
	s.OnEvent(vol(15)) // echo of start
	s.OnEvent(vol(42)) // user moved the Spotify slider
	if s.Volume() != 42 {
		t.Fatalf("volume = %d, want 42", s.Volume())
	}
	if got := saved(t, path); got != "42" {
		t.Fatalf("saved %q, want 42", got)
	}
	if got := f.got(); !equal(got, []int{15}) {
		t.Fatalf("slider change was echoed back: %v", got)
	}
	s.Key(input.KeyPageDown)
	if got := f.got(); !equal(got, []int{15, 37}) {
		t.Fatalf("knob after slider: %v, want [15 37]", got)
	}
}

func TestUnmatchedEventAdoptedAfterTimeout(t *testing.T) {
	now := time.Unix(1000, 0)
	s := New(&fakeSetter{}, filepath.Join(t.TempDir(), "volume"))
	s.now = func() time.Time { return now }
	if err := s.Apply(); err != nil {
		t.Fatal(err)
	}
	s.Step(5)          // 20 in flight, never echoed
	s.OnEvent(vol(15)) // stale, ignored while in flight
	if s.Volume() != 20 {
		t.Fatalf("volume = %d, want 20", s.Volume())
	}
	now = now.Add(5 * time.Second)
	s.OnEvent(vol(60)) // in-flight value expired: this is a real change
	if s.Volume() != 60 {
		t.Fatalf("volume = %d, want 60", s.Volume())
	}
}

func TestEventsBeforeApplyIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "volume")
	f := &fakeSetter{err: soloist.ErrNotConnected}
	s := New(f, path)
	if err := s.Apply(); err == nil {
		t.Fatal("Apply succeeded while not connected")
	}
	s.OnEvent(vol(70)) // Soloist's own value before ours is applied
	f.mu.Lock()
	f.err = nil
	f.mu.Unlock()
	if err := s.Apply(); err != nil {
		t.Fatal(err)
	}
	if got := f.got(); !equal(got, []int{15}) {
		t.Fatalf("sets = %v, want [15]", got)
	}
	if s.Volume() != 15 {
		t.Fatalf("volume = %d, want 15", s.Volume())
	}
}

func TestRunRetriesUntilApplied(t *testing.T) {
	path := filepath.Join(t.TempDir(), "volume")
	f := &fakeSetter{err: soloist.ErrNotConnected}
	s := New(f, path)
	s.retry = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		time.Sleep(20 * time.Millisecond)
		f.mu.Lock()
		f.err = nil
		f.mu.Unlock()
	}()
	s.Run(ctx)
	if got := f.got(); !equal(got, []int{15}) {
		t.Fatalf("sets = %v, want [15]", got)
	}
}

func ptr(v int) *int { return &v }

func TestCompensationUndoesCubicCurve(t *testing.T) {
	for _, c := range []struct {
		v    int
		want float64
	}{{100, 1}, {50, 8}, {15, 296.296}, {0, 0}, {-5, 0}} {
		if got := Compensation(c.v); got < c.want-0.001 || got > c.want+0.001 {
			t.Fatalf("Compensation(%d) = %v, want %v", c.v, got, c.want)
		}
	}
}

func TestOutGetsEveryCurrentVolume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "volume")
	f := &fakeSetter{}
	s := New(f, path)
	var outs []int
	s.Out = func(v int) { outs = append(outs, v) }
	if err := s.Apply(); err != nil {
		t.Fatal(err)
	}
	s.Step(5)          // 20, ours
	s.OnEvent(vol(15)) // stale echo of the start value: no output
	s.OnEvent(vol(20)) // our echo: no output
	s.OnEvent(vol(60)) // the Spotify slider
	if !equal(outs, []int{15, 20, 60}) {
		t.Fatalf("outs = %v, want [15 20 60]", outs)
	}
}

func TestKnobStepDefaultsTo2AndIsSaved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "volume")
	f := &fakeSetter{}
	s := New(f, path)
	if s.KnobStep() != 2 {
		t.Fatalf("default knob step %d, want 2", s.KnobStep())
	}
	s.Apply()
	s.Key(input.KeyPageUp)
	if got := f.got(); !equal(got, []int{15, 17}) {
		t.Fatalf("sets = %v, want [15 17]", got)
	}
	for _, bad := range []int{0, -1, 11} {
		if s.SetKnobStep(bad) == nil {
			t.Fatalf("SetKnobStep(%d) accepted", bad)
		}
	}
	if err := s.SetKnobStep(3); err != nil {
		t.Fatal(err)
	}
	if again := New(f, path); again.KnobStep() != 3 {
		t.Fatalf("reloaded knob step %d, want 3", again.KnobStep())
	}
}

func status(st string) soloist.Event { return soloist.Event{Type: "playback_changed", Status: st} }

// After a pause the HAL resets the amplifier, so resuming hands the volume to
// Resumed again; Soloist is not touched.
func TestResumeCallsResumed(t *testing.T) {
	s, f, _ := newSync(t, "28")
	var mu sync.Mutex
	var got []int
	s.Resumed = func(v int) { mu.Lock(); got = append(got, v); mu.Unlock() }
	s.resumeDelays = []time.Duration{0, 5 * time.Millisecond}
	resumed := func() []int { mu.Lock(); defer mu.Unlock(); return append([]int(nil), got...) }
	s.OnEvent(status("playing"))
	s.OnEvent(status("buffering"))
	s.OnEvent(status("playing")) // track change: not a resume
	s.OnEvent(status("paused"))
	time.Sleep(20 * time.Millisecond)
	if r := resumed(); len(r) != 0 {
		t.Fatalf("resumed before resume: %v", r)
	}
	s.OnEvent(status("buffering"))
	s.OnEvent(status("playing"))
	time.Sleep(20 * time.Millisecond)
	if r := resumed(); !equal(r, []int{28, 28}) {
		t.Fatalf("resumed = %v, want [28 28]", r)
	}
	if sets := f.got(); !equal(sets, []int{28}) {
		t.Fatalf("Soloist sets = %v, want only the start value", sets)
	}
}
