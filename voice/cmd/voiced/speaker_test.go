package main

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// newTestSpeaker returns a speaker whose clock is fake and whose drain wait
// returns at once (recording the requested wait).
func newTestSpeaker(cmd []string, wakes *int) (*speaker, *fakeClock, *time.Duration) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	var waited time.Duration
	s := newSpeaker(cmd, func() { *wakes++ })
	s.now = clk.now
	s.sleep = func(ctx context.Context, d time.Duration) error { waited = d; return ctx.Err() }
	return s, clk, &waited
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for range 400 {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

func TestSpeakerResamplesAndWrites(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	var wakes int
	s, _, waited := newTestSpeaker([]string{"sh", "-c", "cat > " + out}, &wakes)
	defer s.Close()

	pcm := make([]int16, 22050)
	pcm[0], pcm[1] = 16384, -16384
	for i := 2; i < len(pcm); i++ {
		pcm[i] = -32768
	}
	if err := s.Play(context.Background(), pcm, 22050); err != nil {
		t.Fatal(err)
	}
	want := 24000 * 8
	waitFor(t, func() bool { b, _ := os.ReadFile(out); return len(b) >= want*99/100 })
	b, _ := os.ReadFile(out)
	if n := len(b); n < want*99/100 || n > want*101/100 || n%8 != 0 {
		t.Fatalf("wrote %d bytes, want about %d whole stereo frames", n, want)
	}
	f := func(i int) float32 { return math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:])) }
	if f(0) != 0.5 || f(1) != 0.5 {
		t.Fatalf("first frame %v %v, want 0.5 on both channels", f(0), f(1))
	}
	if g := f(2 * 5); g > -0.99 {
		t.Fatalf("later sample %v, want about -1", g)
	}
	if *waited != time.Second+speakerBuffer {
		t.Fatalf("drain wait %v, want 1s + buffer", *waited)
	}
	if wakes != 1 {
		t.Fatalf("wakes %d, want 1", wakes)
	}
}

func TestSpeakerWakeAfterIdle(t *testing.T) {
	var wakes int
	s, clk, _ := newTestSpeaker([]string{"sh", "-c", "cat > /dev/null"}, &wakes)
	defer s.Close()
	pcm := make([]int16, 2400)
	play := func() {
		t.Helper()
		if err := s.Play(context.Background(), pcm, 24000); err != nil {
			t.Fatal(err)
		}
	}
	play()
	clk.advance(500 * time.Millisecond)
	play()
	if wakes != 1 {
		t.Fatalf("wakes %d after plays 0.5 s apart, want 1", wakes)
	}
	clk.advance(2 * time.Second)
	play()
	if wakes != 2 {
		t.Fatalf("wakes %d after a 2 s gap, want 2", wakes)
	}
}

// A write blocked on a backed-up pipe is released by cancel.
func TestSpeakerCancelStops(t *testing.T) {
	var wakes int
	s, _, _ := newTestSpeaker([]string{"sh", "-c", "sleep 30"}, &wakes) // never reads
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() { res <- s.Play(ctx, make([]int16, 24000*5), 24000) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-res:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Play = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Play did not return after cancel")
	}
}

// Cancelling during the wait for the audio to play returns too.
func TestSpeakerCancelDuringDrain(t *testing.T) {
	var wakes int
	s, _, _ := newTestSpeaker([]string{"sh", "-c", "cat > /dev/null"}, &wakes)
	defer s.Close()
	s.sleep = sleepCtx
	s.now = time.Now
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := s.Play(ctx, make([]int16, 24000*5), 24000)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Fatalf("Play = %v after %v, want deadline exceeded soon", err, time.Since(start))
	}
}

func TestSpeakerRestartsAfterDeath(t *testing.T) {
	var wakes int
	s, _, _ := newTestSpeaker([]string{"sh", "-c", "exit 0"}, &wakes)
	defer s.Close()
	pcm := make([]int16, 24000*5)
	if err := s.Play(context.Background(), pcm, 24000); err == nil {
		t.Fatal("Play to a dead helper succeeded")
	}
	s.cmd = []string{"sh", "-c", "cat > /dev/null"}
	if err := s.Play(context.Background(), pcm[:2400], 24000); err != nil {
		t.Fatalf("next Play: %v", err)
	}
	if wakes != 2 {
		t.Fatalf("wakes %d, want 2 (restart wakes again)", wakes)
	}
}
