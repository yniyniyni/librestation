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
	s, _, _ := newTestSpeaker([]string{"sh", "-c", "exec sleep 30"}, &wakes) // never reads
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

// After a cancel nothing more is written, and what was written is whole frames.
func TestSpeakerCancelStopsWriting(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	var wakes int
	s, _, _ := newTestSpeaker([]string{"sh", "-c", "sleep 0.3; cat > " + out}, &wakes)
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() { res <- s.Play(ctx, make([]int16, 24000*5), 24000) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	if err := <-res; !errors.Is(err, context.Canceled) {
		t.Fatalf("Play = %v, want context.Canceled", err)
	}
	time.Sleep(700 * time.Millisecond)
	b1, _ := os.ReadFile(out)
	time.Sleep(200 * time.Millisecond)
	b2, _ := os.ReadFile(out)
	if len(b1) != len(b2) || len(b2)%8 != 0 || len(b2) >= 24000*8*5/2 {
		t.Fatalf("wrote %d then %d bytes, want a stable whole-frame count well under 5 s", len(b1), len(b2))
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
	// Immediately after the failure, with no pause for the reaper.
	if err := s.Play(context.Background(), pcm[:2400], 24000); err != nil {
		t.Fatalf("next Play: %v", err)
	}
	if wakes != 2 {
		t.Fatalf("wakes %d, want 2 (restart wakes again)", wakes)
	}
}

// A helper that stops reading fails the Play within the write timeout, and
// the next Play starts a fresh helper.
func TestSpeakerWedgedHelperTimesOut(t *testing.T) {
	var wakes int
	s, _, _ := newTestSpeaker([]string{"sh", "-c", "exec sleep 30"}, &wakes) // never reads
	defer s.Close()
	s.writeTimeout = 200 * time.Millisecond
	res := make(chan error, 1)
	go func() { res <- s.Play(context.Background(), make([]int16, 24000*5), 24000) }()
	select {
	case err := <-res:
		if err == nil || errors.Is(err, context.Canceled) {
			t.Fatalf("Play = %v, want a write timeout", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Play to a wedged helper did not return")
	}
	s.cmd = []string{"sh", "-c", "cat > /dev/null"}
	if err := s.Play(context.Background(), make([]int16, 2400), 24000); err != nil {
		t.Fatalf("next Play: %v", err)
	}
	if wakes != 2 {
		t.Fatalf("wakes %d, want 2 (the helper was restarted)", wakes)
	}
}

// tickingClock advances by tick on every reading, so time passes while the
// speaker writes, as with a pipe paced by the device.
type tickingClock struct {
	mu   sync.Mutex
	t    time.Time
	tick time.Duration
}

func (c *tickingClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(c.tick)
	return c.t
}

// Play waits from the start of writing: writing that took most of the reply
// does not add to the wait.
func TestPlayWaitsFromStart(t *testing.T) {
	var wakes int
	s, _, waited := newTestSpeaker([]string{"sh", "-c", "cat > /dev/null"}, &wakes)
	defer s.Close()
	clk := &tickingClock{t: time.Unix(1000, 0), tick: speakerChunk}
	s.now = clk.now
	t0 := clk.t
	if err := s.Play(context.Background(), make([]int16, 24000), 24000); err != nil {
		t.Fatal(err)
	}
	done := clk.t.Add(*waited)
	want := time.Second + speakerBuffer
	if got := done.Sub(t0); got < want-3*clk.tick || got > want+3*clk.tick {
		t.Fatalf("Play done %v after start, want about %v", got, want)
	}
}

// The first Live audio of a reply is held back until liveLead has gathered,
// then written at once.
func TestPlayLiveLead(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	var wakes int
	s, _, _ := newTestSpeaker([]string{"sh", "-c", "cat > " + out}, &wakes)
	defer s.Close()
	chunk := make([]int16, 2400) // 100 ms
	if err := s.PlayLive(context.Background(), chunk, 24000); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if b, _ := os.ReadFile(out); len(b) != 0 {
		t.Fatalf("first 100 ms chunk wrote %d bytes, want none yet", len(b))
	}
	if wakes != 0 {
		t.Fatalf("wakes %d before any write, want 0", wakes)
	}
	if err := s.PlayLive(context.Background(), chunk, 24000); err != nil {
		t.Fatal(err)
	}
	want := 4800 * 8
	waitFor(t, func() bool { b, _ := os.ReadFile(out); return len(b) >= want })
	if b, _ := os.ReadFile(out); len(b) != want {
		t.Fatalf("wrote %d bytes, want %d (200 ms)", len(b), want)
	}
	if err := s.PlayLive(context.Background(), make([]int16, 240), 24000); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { b, _ := os.ReadFile(out); return len(b) == want+240*8 })
	if wakes != 1 {
		t.Fatalf("wakes %d, want 1", wakes)
	}
}

// FinishLive waits until everything written since the reply began has played,
// not just the buffer after the last write.
func TestFinishLiveWaitsForAll(t *testing.T) {
	var wakes int
	s, clk, waited := newTestSpeaker([]string{"sh", "-c", "cat > /dev/null"}, &wakes)
	defer s.Close()
	ctx := context.Background()
	play := func(ms int) {
		t.Helper()
		if err := s.PlayLive(ctx, make([]int16, 24*ms), 24000); err != nil {
			t.Fatal(err)
		}
	}
	play(100)
	play(100) // 200 ms written at 0, plays until 0.2 s
	clk.advance(100 * time.Millisecond)
	play(300) // queued behind it: plays until 0.5 s
	clk.advance(100 * time.Millisecond)
	if err := s.FinishLive(ctx); err != nil {
		t.Fatal(err)
	}
	if want := 300*time.Millisecond + speakerBuffer; *waited != want {
		t.Fatalf("FinishLive waited %v, want %v", *waited, want)
	}

	// A short reply still in the lead-in is written by FinishLive.
	*waited = 0
	clk.advance(time.Second)
	play(100)
	if err := s.FinishLive(ctx); err != nil {
		t.Fatal(err)
	}
	if want := 100*time.Millisecond + speakerBuffer; *waited != want {
		t.Fatalf("FinishLive of a lead-in waited %v, want %v", *waited, want)
	}
	// With nothing written it does not wait.
	*waited = 0
	if err := s.FinishLive(ctx); err != nil || *waited != 0 {
		t.Fatalf("empty FinishLive = %v, waited %v; want nil, 0", err, *waited)
	}
}

// realSleep makes s wait in real time and closes the returned channel when a
// wait has begun.
func realSleep(s *speaker) <-chan struct{} {
	started := make(chan struct{})
	var once sync.Once
	s.now = time.Now
	s.sleep = func(ctx context.Context, d time.Duration) error {
		once.Do(func() { close(started) })
		return sleepCtx(ctx, d)
	}
	return started
}

// returnsSoon waits for one result from res and checks it is errFlushed.
func returnsSoon(t *testing.T, what string, res <-chan error, ok ...error) {
	t.Helper()
	select {
	case err := <-res:
		if err != errFlushed && (len(ok) == 0 || err != ok[0]) {
			t.Fatalf("flushed %s = %v, want errFlushed", what, err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("%s did not return after Flush", what)
	}
}

// Flush drops the held-back lead-in and the helper with its queue, releases a
// blocked write and a drain wait at once, and the next PlayLive starts a fresh
// helper.
func TestFlushKillsAndRestarts(t *testing.T) {
	dir := t.TempDir()
	reading := filepath.Join(dir, "reading")
	var wakes int
	// Reads one frame, then never again.
	s, _, waited := newTestSpeaker([]string{"sh", "-c", "head -c 8 > /dev/null; touch " + reading + "; exec sleep 30"}, &wakes)
	defer s.Close()
	ctx := context.Background()

	// The lead-in is dropped: FinishLive after Flush has nothing to wait for.
	if err := s.PlayLive(ctx, make([]int16, 2400), 24000); err != nil {
		t.Fatal(err)
	}
	s.Flush()
	if err := s.FinishLive(ctx); err != nil || *waited != 0 {
		t.Fatalf("FinishLive after Flush = %v, waited %v; want nil, 0", err, *waited)
	}

	// A write stuck on the helper returns at once with errFlushed.
	res := make(chan error, 1)
	go func() { res <- s.PlayLive(ctx, make([]int16, 24000*5), 24000) }()
	waitFor(t, func() bool { _, err := os.Stat(reading); return err == nil })
	start := time.Now()
	s.Flush()
	returnsSoon(t, "PlayLive", res)
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("Flush took %v", d)
	}
	if s.proc != nil {
		t.Fatal("helper kept after Flush")
	}

	// The next reply starts a fresh helper and wakes the output again.
	started := realSleep(s)
	out := filepath.Join(dir, "out")
	s.cmd = []string{"sh", "-c", "cat > " + out}
	if err := s.PlayLive(ctx, make([]int16, 4800), 24000); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { b, _ := os.ReadFile(out); return len(b) == 4800*8 })
	if wakes != 2 {
		t.Fatalf("wakes %d, want 2 (Flush restarts the helper)", wakes)
	}

	// The wait for a reply to play out ends at once too.
	go func() { res <- s.FinishLive(ctx) }()
	<-started
	s.Flush()
	returnsSoon(t, "FinishLive", res)
}

// Flush interrupts a regular Play waiting for its audio to play.
func TestFlushInterruptsPlay(t *testing.T) {
	var wakes int
	s, _, _ := newTestSpeaker([]string{"sh", "-c", "cat > /dev/null"}, &wakes)
	defer s.Close()
	started := realSleep(s)
	res := make(chan error, 1)
	go func() { res <- s.Play(context.Background(), make([]int16, 24000*5), 24000) }()
	<-started
	s.Flush()
	returnsSoon(t, "Play", res)
}

// A Flush that lands between two calls is not waited out by the next one:
// a FinishLive that takes the lock before the Flush's kill returns at once
// instead of draining the reply.
func TestFlushBetweenCalls(t *testing.T) {
	var wakes int
	s, _, _ := newTestSpeaker([]string{"sh", "-c", "cat > /dev/null"}, &wakes)
	defer s.Close()
	started := realSleep(s)
	ctx := context.Background()
	if err := s.PlayLive(ctx, make([]int16, 4800), 24000); err != nil {
		t.Fatal(err)
	}

	s.play.Lock() // a call between done and Unlock: nothing for Flush to cancel
	locked := true
	defer func() {
		if locked {
			s.play.Unlock()
		}
	}()
	s.liveEnd = time.Now().Add(5 * time.Second)
	flushed := make(chan struct{})
	go func() { s.Flush(); close(flushed) }()
	waitFor(t, func() bool { s.op.Lock(); defer s.op.Unlock(); return s.flushing > 0 })
	// A call that gets the lock before the Flush's kill starts cancelled.
	opCtx, done := s.begin(ctx)
	if context.Cause(opCtx) != errFlushed {
		t.Fatalf("call begun during a Flush: cause %v, want errFlushed", context.Cause(opCtx))
	}
	var err error = opCtx.Err()
	done(&err)
	if err != errFlushed {
		t.Fatalf("call begun during a Flush returned %v, want errFlushed", err)
	}
	res := make(chan error, 1)
	go func() { res <- s.FinishLive(ctx) }()
	locked = false
	s.play.Unlock()

	// Whichever takes the lock first, neither waits out the reply.
	returnsSoon(t, "FinishLive", res, nil)
	select {
	case <-flushed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Flush waited out the next call")
	}
	select {
	case <-started:
		t.Fatal("FinishLive drained after a Flush")
	default:
	}
}
