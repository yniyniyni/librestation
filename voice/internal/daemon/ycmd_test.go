package daemon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	miccap "voice/internal/capture"
)

// cmdTest is a daemon with a fake assistant (idle), music playing and a
// settable clock; voiced started an hour ago. Word "x" pauses.
func cmdTest(t *testing.T) (*Daemon, *fakeAct, *fakeAssistant, *time.Time, chan struct{}) {
	t.Helper()
	f := &fakeAct{}
	d, done := newTest(t, f, "pause", false)
	a := &fakeAssistant{}
	d.opt.Assistant = a
	clock := time.Unix(100000, 0)
	d.now = func() time.Time { return clock }
	d.started = clock.Add(-time.Hour)
	d.ObservePlayback("playing")
	return d, f, a, &clock, done
}

func say(d *Daemon, cmd string) {
	d.mu.Lock()
	d.onCommand(cmd, 0.9)
	d.mu.Unlock()
}

// lastHit is the newest hit; it fails the test when there is none.
func lastHit(t *testing.T, d *Daemon) Hit {
	t.Helper()
	hits := d.state().Hits
	if len(hits) == 0 {
		t.Fatal("no hit recorded")
	}
	return hits[0]
}

// idle waits a moment and checks that nothing was run.
func idle(t *testing.T, f *fakeAct, a *fakeAssistant) {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	if g := f.got(); len(g) != 0 || a.hushed() != 0 {
		t.Fatalf("actuator %v, hushes %d", g, a.hushed())
	}
}

func TestCommandWhileListeningIgnored(t *testing.T) {
	for _, st := range []string{"listening", "thinking", "followup"} {
		t.Run(st, func(t *testing.T) {
			d, f, a, _, _ := cmdTest(t)
			a.setState(st)
			say(d, "стоп")
			idle(t, f, a)
			if h := lastHit(t, d); h.Fired || !h.Debounced || h.Source != "yandex" {
				t.Fatalf("hit %+v", h)
			}
		})
	}
}

func TestStopWhileSpeakingHushes(t *testing.T) {
	d, f, a, _, done := cmdTest(t)
	a.setState("speaking")
	d.yring.write(make([]int16, 16000))
	say(d, "хватит")
	<-done
	if a.hushed() != 1 || len(f.got()) != 0 {
		t.Fatalf("hushes %d, actuator %v", a.hushed(), f.got())
	}
	h := lastHit(t, d)
	if !h.Fired || h.Action != "hush" || h.Word != "хватит" || h.Name != "хватит" || h.Source != "yandex" {
		t.Fatalf("hit %+v", h)
	}
	d.mu.Lock()
	n := len(d.hits[0].audio)
	d.mu.Unlock()
	if n != 16000 {
		t.Fatalf("clip %d samples", n)
	}
}

func TestNextWhileSpeakingIgnored(t *testing.T) {
	d, f, a, _, _ := cmdTest(t)
	a.setState("speaking")
	say(d, "дальше")
	idle(t, f, a)
	if h := lastHit(t, d); h.Fired || !h.Debounced {
		t.Fatalf("hit %+v", h)
	}
}

func TestPlayingActions(t *testing.T) {
	for _, c := range []struct{ cmd, want string }{
		{"стоп", "pause"}, {"хватит", "pause"}, {"пауза", "pause"},
		{"дальше", "next"}, {"следующий", "next"}, {"назад", "prev"},
		{"громче", "louder"}, {"тише", "quieter"}, {"продолжи", ""},
	} {
		t.Run(c.cmd, func(t *testing.T) {
			d, f, a, _, done := cmdTest(t)
			say(d, c.cmd)
			h := lastHit(t, d)
			if c.want == "" {
				idle(t, f, a)
				if h.Fired || !h.Debounced {
					t.Fatalf("hit %+v", h)
				}
				return
			}
			<-done
			if g := f.got(); !slices.Equal(g, []string{c.want}) {
				t.Fatalf("actuator %v", g)
			}
			if !h.Fired || h.Debounced || h.Action != c.want || !h.Playback {
				t.Fatalf("hit %+v", h)
			}
		})
	}
	d, f, a, _, _ := cmdTest(t)
	say(d, "включи свет")
	say(d, "играй")
	idle(t, f, a)
	if n := len(d.state().Hits); n != 0 {
		t.Fatalf("%d hits for unused commands", n)
	}
}

func TestContinueWindow(t *testing.T) {
	d, f, _, clock, done := cmdTest(t)
	d.ObservePlayback("paused")
	*clock = clock.Add(12 * time.Minute)
	say(d, "дальше") // not playing: only "продолжи" acts
	if h := lastHit(t, d); h.Fired || h.Debounced {
		t.Fatalf("next while paused: %+v", h)
	}
	say(d, "продолжи")
	<-done
	if g := f.got(); !slices.Equal(g, []string{"play"}) {
		t.Fatalf("12 min: actuator %v", g)
	}
	*clock = clock.Add(4 * time.Minute) // 16 min since it played
	say(d, "продолжи")
	time.Sleep(50 * time.Millisecond)
	if g := f.got(); len(g) != 1 {
		t.Fatalf("16 min: actuator %v", g)
	}
	if h := lastHit(t, d); h.Fired {
		t.Fatalf("16 min: hit %+v", h)
	}

	// A long play counts until it stops.
	d.ObservePlayback("playing")
	*clock = clock.Add(time.Hour)
	d.ObservePlayback("paused")
	*clock = clock.Add(5 * time.Minute)
	say(d, "продолжи")
	<-done
	if g := f.got(); len(g) != 2 {
		t.Fatalf("after a long play: actuator %v", g)
	}

	// Never played, voiced started 5 minutes ago.
	d2, f2, _, clock2, done2 := cmdTest(t)
	d2.ObservePlayback("paused")
	d2.lastPlaying = time.Time{}
	d2.started = clock2.Add(-5 * time.Minute)
	say(d2, "продолжи")
	<-done2
	if g := f2.got(); !slices.Equal(g, []string{"play"}) {
		t.Fatalf("after start: actuator %v", g)
	}
}

func TestQuickOffAndGroupOff(t *testing.T) {
	d, f, a, _, done := cmdTest(t)
	d.settings.Quick.Off = true
	say(d, "стоп")
	idle(t, f, a)
	if n := len(d.state().Hits); n != 0 {
		t.Fatalf("off: %d hits", n)
	}
	d.settings.Quick = QuickSettings{Disabled: map[string]bool{"volume": true}}
	say(d, "громче")
	idle(t, f, a)
	if n := len(d.state().Hits); n != 0 {
		t.Fatalf("volume off: %d hits", n)
	}
	say(d, "стоп")
	<-done
	if g := f.got(); !slices.Equal(g, []string{"pause"}) {
		t.Fatalf("actuator %v", g)
	}
}

func TestDryRun(t *testing.T) {
	d, f, a, _, _ := cmdTest(t)
	d.settings.DryRun = true
	say(d, "стоп")
	a.setState("speaking")
	say(d, "стоп")
	idle(t, f, a)
	hits := d.state().Hits
	if len(hits) != 2 || hits[0].Fired || hits[1].Fired {
		t.Fatalf("hits %+v", hits)
	}
}

func TestYandexFalseNoWordDir(t *testing.T) {
	d, _, _, _, done := cmdTest(t)
	d.yring.write(make([]int16, 16000))
	words := func() []string {
		e, _ := os.ReadDir(filepath.Join(d.opt.DataDir, "words"))
		var n []string
		for _, x := range e {
			n = append(n, x.Name())
		}
		return n
	}
	before, falses := words(), d.newFalses("x")
	say(d, "стоп")
	<-done
	d.mu.Lock()
	h := d.hits[len(d.hits)-1]
	d.mu.Unlock()
	if err := d.setVerdict(h, "false"); err != nil {
		t.Fatal(err)
	}
	d.waitIO()
	if got := lastHit(t, d); got.Verdict != "false" || got.FalseFile != "" {
		t.Fatalf("hit %+v", got)
	}
	if after := words(); !slices.Equal(after, before) {
		t.Fatalf("words %v, was %v", after, before)
	}
	if n := d.newFalses("x"); n != falses {
		t.Fatalf("newFalses %d, was %d", n, falses)
	}
	if n := d.newFalses("стоп"); n != 0 {
		t.Fatalf("newFalses(стоп) %d", n)
	}
}

func TestNoModel(t *testing.T) {
	d, err := New(Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	d.spotCommands(make([]int16, miccap.BlockFrames*miccap.Channels))
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { d.Run(ctx); close(stopped) }()
	cancel()
	<-stopped
	if n := len(d.state().Hits); n != 0 {
		t.Fatalf("%d hits", n)
	}
}

func TestDebounceShared(t *testing.T) {
	d, f, a, clock, done := cmdTest(t)
	fireWord(d, "x") // our word, action pause
	<-done
	*clock = clock.Add(100 * time.Millisecond)
	say(d, "стоп")
	time.Sleep(50 * time.Millisecond)
	if g := f.got(); len(g) != 1 || a.hushed() != 0 {
		t.Fatalf("actuator %v", g)
	}
	if h := lastHit(t, d); h.Fired || !h.Debounced {
		t.Fatalf("hit %+v", h)
	}
}
