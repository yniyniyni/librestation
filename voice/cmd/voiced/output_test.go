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
	wait("32\n")
	o.set(32) // unchanged: nothing to write
	time.Sleep(30 * time.Millisecond)
	wait("32\n")
	o.refresh(32)
	wait("32\n32\n")
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
