package main

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"voice/internal/alarms"
	"voice/internal/led/proto"
)

// ringRig is a ringer over fakes that note what they are asked, in order.
type ringRig struct {
	t  *testing.T
	r  *ringer
	mu sync.Mutex
	// log holds "hush", "pause", "resume", "flush", "play", "music", "hold N".
	log      []string
	playing  bool
	playErr  error
	musicErr error
	leds     []proto.Event
	clock    time.Time
	ticks    chan time.Time
	started  chan struct{}
	vol      int
}

func newRingRig(t *testing.T) *ringRig {
	g := &ringRig{t: t, ticks: make(chan time.Time), started: make(chan struct{}, 100), vol: 40,
		clock: time.Date(2026, 11, 6, 7, 0, 0, 0, time.UTC)}
	note := func(s string) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.log = append(g.log, s)
	}
	g.r = &ringer{
		sounds: t.TempDir(),
		play: func(ctx context.Context, pcm []int16, rate int) error {
			if len(pcm) == 0 || rate <= 0 {
				t.Error("nothing to play")
			}
			note("play")
			g.started <- struct{}{}
			g.mu.Lock()
			err := g.playErr
			g.mu.Unlock()
			if err != nil {
				return err
			}
			<-ctx.Done()
			return ctx.Err()
		},
		flush: func() { note("flush") },
		hold:  func(v int) { note("hold " + strconv.Itoa(v)) },
		volume: func() int {
			g.mu.Lock()
			defer g.mu.Unlock()
			return g.vol
		},
		playing: func() bool {
			g.mu.Lock()
			defer g.mu.Unlock()
			return g.playing
		},
		pause: func() error {
			note("pause")
			g.mu.Lock()
			defer g.mu.Unlock()
			g.playing = false
			return nil
		},
		resume: func() error { note("resume"); return nil },
		hush:   func() { note("hush") },
		music: func(ctx context.Context, q string) error {
			note("music " + q)
			g.mu.Lock()
			defer g.mu.Unlock()
			return g.musicErr
		},
		led: func(e proto.Event) {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.leds = append(g.leds, e)
		},
		now: func() time.Time {
			g.mu.Lock()
			defer g.mu.Unlock()
			return g.clock
		},
		sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() },
		ticks: func() (<-chan time.Time, func()) { return g.ticks, func() {} },
	}
	return g
}

// tick moves the clock and has the ring loop take the moment.
func (g *ringRig) tick(d time.Duration) {
	g.t.Helper()
	g.mu.Lock()
	g.clock = g.clock.Add(d)
	now := g.clock
	g.mu.Unlock()
	select {
	case g.ticks <- now:
	case <-time.After(5 * time.Second):
		g.t.Fatal("the ring loop took no tick")
	}
	// A second send proves the first was handled before the test looks.
	g.mu.Lock()
	now = g.clock
	g.mu.Unlock()
	select {
	case g.ticks <- now:
	case <-time.After(5 * time.Second):
		g.t.Fatal("the ring loop took no tick")
	}
}

func (g *ringRig) waitPlay() {
	g.t.Helper()
	select {
	case <-g.started:
	case <-time.After(5 * time.Second):
		g.t.Fatal("no sound")
	}
}

func (g *ringRig) calls() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.log)
}

// wait stops the ring and waits for the loop to finish.
func (g *ringRig) stop() {
	g.t.Helper()
	g.r.Stop(false)
	g.r.wait()
}

func has(calls []string, s string) bool { return slices.Contains(calls, s) }

func before(t *testing.T, calls []string, a, b string) {
	t.Helper()
	i, j := slices.Index(calls, a), slices.Index(calls, b)
	if i < 0 || j < 0 || i >= j {
		t.Errorf("want %q before %q: %v", a, b, calls)
	}
}

func TestAlarmRampsFromTen(t *testing.T) {
	g := newRingRig(t)
	g.r.Start(alarms.Ring{Kind: alarms.KindAlarm})
	g.waitPlay()
	c := g.calls()
	if !has(c, "hush") || !has(c, "hold 10") {
		t.Fatalf("%v", c)
	}
	if has(c, "pause") {
		t.Errorf("paused with no music: %v", c)
	}
	g.tick(19 * time.Second)
	if has(g.calls(), "hold 20") {
		t.Error("rose early")
	}
	g.tick(time.Second)
	if !has(g.calls(), "hold 20") {
		t.Errorf("no rise: %v", g.calls())
	}
	g.stop()
	c = g.calls()
	before(t, c, "flush", "hold -1")
	if has(c, "resume") {
		t.Errorf("resumed nothing: %v", c)
	}
}

func TestMusicPausedAndResumed(t *testing.T) {
	for _, snooze := range []bool{false, true} {
		g := newRingRig(t)
		g.playing = true
		g.r.Start(alarms.Ring{Kind: alarms.KindAlarm})
		g.waitPlay()
		before(t, g.calls(), "pause", "play")
		g.r.Stop(snooze)
		g.r.wait()
		before(t, g.calls(), "hold -1", "resume")
	}
}

func TestTimerHoldsCurrentVolume(t *testing.T) {
	for _, c := range []struct{ vol, want int }{{40, 40}, {10, 30}, {30, 30}} {
		g := newRingRig(t)
		g.vol = c.vol
		g.r.Start(alarms.Ring{Kind: alarms.KindTimer, Sound: alarms.TimerSound})
		g.waitPlay()
		g.tick(40 * time.Second)
		holds := 0
		for _, s := range g.calls() {
			if len(s) > 5 && s[:5] == "hold " {
				holds++
			}
		}
		if !has(g.calls(), "hold "+strconv.Itoa(c.want)) || holds != 1 {
			t.Errorf("volume %d: %v", c.vol, g.calls())
		}
		g.stop()
	}
}

func TestMusicAlarm(t *testing.T) {
	g := newRingRig(t)
	g.r.Start(alarms.Ring{Kind: alarms.KindAlarm, Music: "The Weeknd"})
	for !has(g.calls(), "music The Weeknd") {
		time.Sleep(time.Millisecond)
	}
	g.mu.Lock()
	g.playing = true
	g.mu.Unlock()
	g.tick(time.Second)
	g.tick(30 * time.Second)
	c := g.calls()
	if has(c, "play") {
		t.Errorf("rang over the music: %v", c)
	}
	if !has(c, "hold 20") {
		t.Errorf("music does not ramp: %v", c)
	}
	g.stop()
	c = g.calls()
	if !has(c, "pause") || has(c, "resume") {
		t.Errorf("stop: %v", c)
	}
}

func TestMusicFailsToSound(t *testing.T) {
	g := newRingRig(t)
	g.musicErr = errors.New("no Spotify")
	g.r.Start(alarms.Ring{Kind: alarms.KindAlarm, Music: "x"})
	g.waitPlay()
	g.stop()
}

func TestMusicSilentFallsToSound(t *testing.T) {
	g := newRingRig(t)
	g.r.Start(alarms.Ring{Kind: alarms.KindAlarm, Music: "x"})
	for !has(g.calls(), "music x") {
		time.Sleep(time.Millisecond)
	}
	g.tick(musicWait - time.Second)
	if has(g.calls(), "play") {
		t.Fatal("gave up early")
	}
	g.tick(time.Second)
	g.waitPlay()
	g.stop()
}

// What played before a music alarm that then failed is paused for the ring and
// comes back after it.
func TestMusicFailsOverPlayingMusic(t *testing.T) {
	g := newRingRig(t)
	g.playing = true
	g.musicErr = errors.New("no Spotify")
	g.r.Start(alarms.Ring{Kind: alarms.KindAlarm, Music: "x"})
	g.waitPlay()
	before(t, g.calls(), "pause", "play")
	g.stop()
	if !has(g.calls(), "resume") {
		t.Errorf("%v", g.calls())
	}
}

func TestSoundErrorKeepsTrying(t *testing.T) {
	g := newRingRig(t)
	g.playErr = errors.New("no output")
	g.r.Start(alarms.Ring{Kind: alarms.KindAlarm})
	g.waitPlay()
	g.waitPlay()
	g.waitPlay()
	g.mu.Lock()
	g.playErr = nil
	g.mu.Unlock()
	g.stop()
}

func TestRingShownAndCleared(t *testing.T) {
	g := newRingRig(t)
	g.r.Start(alarms.Ring{Kind: alarms.KindTimer, Sound: alarms.TimerSound})
	g.waitPlay()
	g.tick(time.Second)
	g.stop()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.leds) < 3 || g.leds[0].T != "ring" || g.leds[0].A != "timer" {
		t.Fatalf("%+v", g.leds)
	}
	if last := g.leds[len(g.leds)-1]; last.T != "ring" || last.A != "" {
		t.Errorf("last %+v", last)
	}
}

// Rings one after another do not overlap: the second waits for the first to end.
func TestRingsDoNotOverlap(t *testing.T) {
	g := newRingRig(t)
	g.r.Start(alarms.Ring{Kind: alarms.KindAlarm})
	g.waitPlay()
	g.r.Stop(false)
	g.r.Start(alarms.Ring{Kind: alarms.KindAlarm})
	g.waitPlay()
	g.stop()
	c := g.calls()
	if slices.Index(c, "hold -1") > slices.Index(c[1:], "hush")+1 {
		t.Errorf("%v", c)
	}
}

// Optional parts missing: no assistant, no Spotify.
func TestRingWithoutOptionalParts(t *testing.T) {
	g := newRingRig(t)
	g.r.hush, g.r.music, g.r.led = nil, nil, nil
	g.r.Start(alarms.Ring{Kind: alarms.KindAlarm, Music: "x"})
	g.waitPlay()
	g.stop()
}
