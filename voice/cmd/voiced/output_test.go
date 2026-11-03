package main

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"voice/internal/volume"
)

type lines struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// The audio HAL resets the amplifier when it wakes from standby; refresh
// writes the same volume again although the helper was already given it.
func TestRefreshRewritesSameVolume(t *testing.T) {
	o := newOutput(nil, "")
	var w lines
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { o.ramp(ctx, &w, -1); close(done) }()
	defer func() { cancel(); <-done }()

	wait := func(want string) {
		t.Helper()
		for range 200 {
			if w.String() == want {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("helper got %q, want %q", w.String(), want)
	}
	o.set(32)
	wait("31\n32\n") // the first value too: the HAL may believe it is set
	o.set(32)        // unchanged: nothing to write
	time.Sleep(30 * time.Millisecond)
	wait("31\n32\n")
	o.refresh(32)
	wait("31\n32\n31\n32\n") // a neighbour first, or the HAL drops the write
}

func TestDuckScalesGain(t *testing.T) {
	gain := filepath.Join(t.TempDir(), "gain")
	o := newOutput(nil, gain)
	read := func() float64 {
		t.Helper()
		b, err := os.ReadFile(gain)
		if err != nil {
			t.Fatal(err)
		}
		g, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	check := func(v int, f float64) {
		t.Helper()
		want := volume.Compensation(v) * f
		if got := read(); math.Abs(got-want) > 1e-6*want {
			t.Fatalf("gain %v, want %v", got, want)
		}
	}
	o.duck(0.2) // before any volume is known: nothing to write, factor kept
	o.duck(1)
	o.set(50)
	check(50, 1)
	o.duck(0.2)
	check(50, 0.2)
	o.set(60)
	check(60, 0.2)
	o.duck(1)
	check(60, 1)
	o.duck(7) // clamped
	check(60, 1)
}

func TestAmpWatchSetsItBack(t *testing.T) {
	o := newOutput(nil, "")
	o.mu.Lock()
	o.target, o.force = 27, false
	o.mu.Unlock()
	var w ampWatch
	now := time.Now()
	o.checkAmp(&w, 132, now) // what volume 27 makes
	o.checkAmp(&w, 132, now.Add(ampEvery))
	if _, force := o.wanted(); force {
		t.Fatal("nothing changed, yet it is set again")
	}
	o.checkAmp(&w, 109, now.Add(2*ampEvery)) // the HAL at a track change
	if v, force := o.wanted(); !force || v != 27 {
		t.Errorf("wanted %d, force %v; want 27 written again", v, force)
	}
}

func TestAmpWatchTriesSeldomWhenItDoesNotTake(t *testing.T) {
	o := newOutput(nil, "")
	o.mu.Lock()
	o.target, o.force = 27, false
	o.mu.Unlock()
	var w ampWatch
	now := time.Now()
	o.checkAmp(&w, 132, now)
	tries := 0
	for range 2 * ampRetryEvery {
		now = now.Add(ampEvery)
		o.checkAmp(&w, 100, now)
		if _, force := o.wanted(); force {
			tries++
		}
	}
	if tries > ampTries+2 {
		t.Errorf("%d tries", tries)
	}
}

func TestAmpWatchLearnsANewVolume(t *testing.T) {
	o := newOutput(nil, "")
	o.mu.Lock()
	o.target, o.force = 27, false
	o.mu.Unlock()
	var w ampWatch
	now := time.Now()
	o.checkAmp(&w, 132, now)
	o.mu.Lock()
	o.target, o.wrote = 40, now
	o.mu.Unlock()
	o.checkAmp(&w, 150, now.Add(ampSettle/2)) // on its way
	o.checkAmp(&w, 160, now.Add(ampSettle))   // there
	o.checkAmp(&w, 160, now.Add(ampSettle+ampEvery))
	if _, force := o.wanted(); force {
		t.Error("a volume change taken for the HAL's")
	}
}

func TestHoldOverridesVolume(t *testing.T) {
	o := newOutput(nil, "")
	o.set(40)
	o.wanted()
	o.hold(10)
	if v, _ := o.wanted(); v != 10 {
		t.Fatalf("held: wanted %d", v)
	}
	o.set(55) // the user's volume moves on behind the ring
	if v, _ := o.wanted(); v != 10 {
		t.Errorf("after set: wanted %d", v)
	}
	o.refresh(55) // the HAL woke; the amplifier goes back to what is wanted now
	if v, force := o.wanted(); v != 10 || !force {
		t.Errorf("after refresh: wanted %d force %v", v, force)
	}
	o.hold(-1)
	if v, _ := o.wanted(); v != 55 {
		t.Errorf("released: wanted %d", v)
	}
}

func TestHoldBeforeAnyVolume(t *testing.T) {
	o := newOutput(nil, "")
	o.hold(10)
	o.hold(-1)
	if v, _ := o.wanted(); v != 10 {
		t.Errorf("wanted %d: no volume to go back to, stay", v)
	}
	o.set(30)
	if v, _ := o.wanted(); v != 30 {
		t.Errorf("wanted %d", v)
	}
}

// The amplifier watch must keep the ring's level, not fight it.
func TestAmpWatchKeepsHold(t *testing.T) {
	o := newOutput(nil, "")
	o.set(40)
	o.hold(10)
	o.wanted()
	var w ampWatch
	now := time.Now().Add(time.Second)
	o.checkAmp(&w, 100, now)
	if w.volume != 10 {
		t.Errorf("watching volume %d", w.volume)
	}
}
