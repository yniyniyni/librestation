package capture

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type chunkReader struct {
	data []byte
	n    int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := c.n
	if n > len(p) {
		n = len(p)
	}
	if n > len(c.data) {
		n = len(c.data)
	}
	copy(p, c.data[:n])
	c.data = c.data[n:]
	return n, nil
}

func makeBytes(blocks int) ([]byte, []int16) {
	samples := make([]int16, blocks*BlockFrames*Channels)
	for i := range samples {
		samples[i] = int16(i*7 - 3000)
	}
	buf := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(s))
	}
	return buf, samples
}

func TestReadBlocksShortReads(t *testing.T) {
	buf, want := makeBytes(3)
	var got []int16
	n := 0
	err := ReadBlocks(&chunkReader{data: buf, n: 7}, func(b []int16) error {
		if len(b) != BlockFrames*Channels {
			t.Fatalf("block len %d", len(b))
		}
		got = append(got, b...)
		n++
		return nil
	})
	if err != io.EOF {
		t.Fatalf("err = %v, want EOF", err)
	}
	if n != 3 || len(got) != len(want) {
		t.Fatalf("blocks=%d samples=%d", n, len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d: %d != %d", i, got[i], want[i])
		}
	}
}

func TestReadBlocksPartialTailIsEOF(t *testing.T) {
	buf, _ := makeBytes(2)
	buf = buf[:len(buf)-10]
	n := 0
	err := ReadBlocks(bytes.NewReader(buf), func([]int16) error { n++; return nil })
	if err != io.EOF {
		t.Fatalf("err = %v, want EOF", err)
	}
	if n != 1 {
		t.Fatalf("blocks = %d, want 1", n)
	}
}

func TestReadBlocksCallbackError(t *testing.T) {
	buf, _ := makeBytes(2)
	stop := io.ErrClosedPipe
	err := ReadBlocks(bytes.NewReader(buf), func([]int16) error { return stop })
	if err != stop {
		t.Fatalf("err = %v", err)
	}
}

func TestRunHelperRestartsOnEOF(t *testing.T) {
	defer func(a, b time.Duration) { backoffMin, backoffMax = a, b }(backoffMin, backoffMax)
	backoffMin, backoffMax = 10*time.Millisecond, 40*time.Millisecond
	var blocks atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r := &Runner{
		Cmd: []string{"sh", "-c", "echo 'mic: started session=1' >&2; head -c 61440 /dev/zero"},
		OnBlock: func([]int16) {
			if blocks.Add(1) >= 4 { // 2 blocks per start => >= 2 starts
				cancel()
			}
		},
	}
	err := r.Run(ctx)
	if err == nil {
		t.Fatal("Run should return ctx error")
	}
	if blocks.Load() < 4 {
		t.Fatalf("blocks = %d, want >= 4 (restart)", blocks.Load())
	}
}

func TestSilenceDetector(t *testing.T) {
	d := newSilenceDetector(250)
	zero := make([]int16, BlockFrames*Channels)
	loud := make([]int16, BlockFrames*Channels)
	loud[5000] = 1
	fired := 0
	for i := 0; i < 249; i++ {
		if d.feed(zero) {
			fired++
		}
	}
	if fired != 0 {
		t.Fatal("fired early")
	}
	if d.feed(loud) {
		t.Fatal("loud block fired")
	}
	for i := 0; i < 249; i++ {
		if d.feed(zero) {
			t.Fatal("counter was not reset by non-zero block")
		}
	}
	if !d.feed(zero) {
		t.Fatal("should fire at 250th zero block")
	}
	if d.feed(zero) {
		t.Fatal("must not fire again immediately")
	}
}

func TestSilenceWatchdogTriggers(t *testing.T) {
	defer func(a, b time.Duration) { backoffMin, backoffMax = a, b }(backoffMin, backoffMax)
	backoffMin, backoffMax = time.Hour, time.Hour // no restart within the test
	var mu sync.Mutex
	silence := 0
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// 250 blocks of zeros = 5 s of audio.
	r := &Runner{
		Cmd: []string{"sh", "-c", "head -c 7680000 /dev/zero"},
		OnSilence: func() {
			mu.Lock()
			silence++
			mu.Unlock()
			cancel()
		},
	}
	_ = r.Run(ctx)
	mu.Lock()
	defer mu.Unlock()
	if silence != 1 {
		t.Fatalf("OnSilence called %d times, want 1", silence)
	}
}

func TestSilenceRestartsHelper(t *testing.T) {
	defer func(a, b time.Duration) { backoffMin, backoffMax = a, b }(backoffMin, backoffMax)
	backoffMin, backoffMax = 10*time.Millisecond, 40*time.Millisecond
	starts := filepath.Join(t.TempDir(), "starts")
	var silence atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r := &Runner{
		Cmd: []string{"sh", "-c", "echo x >> " + starts + "; cat /dev/zero"},
		OnSilence: func() {
			if silence.Add(1) >= 3 {
				cancel()
			}
		},
	}
	_ = r.Run(ctx)
	if silence.Load() < 3 {
		t.Fatalf("OnSilence called %d times, want >= 3", silence.Load())
	}
	b, _ := os.ReadFile(starts)
	if n := strings.Count(string(b), "x"); n < 2 {
		t.Fatalf("helper started %d times, want >= 2", n)
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func countStarts(path string) int {
	b, _ := os.ReadFile(path)
	return strings.Count(string(b), "x")
}

func TestMuteStopsHelperUntilUnmuted(t *testing.T) {
	defer func(a, b, c time.Duration) { backoffMin, backoffMax, mutePoll = a, b, c }(backoffMin, backoffMax, mutePoll)
	backoffMin, backoffMax, mutePoll = 10*time.Millisecond, 40*time.Millisecond, 5*time.Millisecond
	starts := filepath.Join(t.TempDir(), "starts")
	var muted atomic.Bool
	var onMuted, silence atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	r := &Runner{
		Cmd:       []string{"sh", "-c", "echo x >> " + starts + "; exec cat /dev/zero"},
		Muted:     muted.Load,
		OnMuted:   func() { onMuted.Add(1) },
		OnSilence: func() { silence.Add(1) },
	}
	muted.Store(false)
	go func() { done <- r.Run(ctx) }()
	waitFor(t, "first start", func() bool { return countStarts(starts) == 1 })
	muted.Store(true)
	waitFor(t, "OnMuted", func() bool { return onMuted.Load() == 1 })

	silenceBefore := silence.Load()
	time.Sleep(200 * time.Millisecond) // many poll periods and silence windows
	if n := countStarts(starts); n != 1 {
		t.Fatalf("helper started %d times while muted, want 1", n)
	}
	if onMuted.Load() != 1 {
		t.Fatalf("OnMuted called %d times, want 1", onMuted.Load())
	}
	if silence.Load() != silenceBefore {
		t.Fatal("OnSilence called while muted")
	}

	muted.Store(false)
	waitFor(t, "start after unmute", func() bool { return countStarts(starts) >= 2 })
	cancel()
	if err := <-done; err == nil {
		t.Fatal("Run should return ctx error")
	}
}

func TestMutedAtStartDoesNotStartHelper(t *testing.T) {
	defer func(c time.Duration) { mutePoll = c }(mutePoll)
	mutePoll = 5 * time.Millisecond
	starts := filepath.Join(t.TempDir(), "starts")
	var onMuted atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	r := &Runner{
		Cmd:     []string{"sh", "-c", "echo x >> " + starts + "; exec cat /dev/zero"},
		Muted:   func() bool { return true },
		OnMuted: func() { onMuted.Add(1) },
	}
	if err := r.Run(ctx); err == nil {
		t.Fatal("Run should return ctx error")
	}
	if n := countStarts(starts); n != 0 {
		t.Fatalf("helper started %d times, want 0", n)
	}
	if onMuted.Load() != 1 {
		t.Fatalf("OnMuted called %d times, want 1", onMuted.Load())
	}
}

// A mic that stays silent with the switch off (or no switch to read) still
// gets restarted, but less and less often.
func TestSilencePauseGrows(t *testing.T) {
	defer func(a, b time.Duration) { backoffMin, backoffMax = a, b }(backoffMin, backoffMax)
	backoffMin, backoffMax = 50*time.Millisecond, 200*time.Millisecond
	var at []time.Time
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r := &Runner{
		Cmd: []string{"sh", "-c", "exec cat /dev/zero"},
		OnSilence: func() { // one goroutine at a time: restarts are sequential
			if at = append(at, time.Now()); len(at) == 5 {
				cancel()
			}
		},
	}
	_ = r.Run(ctx)
	if len(at) < 5 {
		t.Fatalf("OnSilence called %d times, want 5", len(at))
	}
	for i, want := range []time.Duration{50, 100, 200, 200} {
		want *= time.Millisecond
		if gap := at[i+1].Sub(at[i]); gap < want {
			t.Fatalf("gap %d = %s, want >= %s", i, gap, want)
		}
	}
}

// Hearing anything between silences means the mic works, so the pause restarts
// from the minimum: with doubling this many silences would outlast the timeout.
func TestHeardSoundResetsSilencePause(t *testing.T) {
	defer func(a, b time.Duration) { backoffMin, backoffMax = a, b }(backoffMin, backoffMax)
	backoffMin, backoffMax = 10*time.Millisecond, time.Hour
	var silence atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r := &Runner{
		Cmd: []string{"sh", "-c", `printf '\001\000'; exec cat /dev/zero`},
		OnSilence: func() {
			if silence.Add(1) == 12 {
				cancel()
			}
		},
	}
	_ = r.Run(ctx)
	if n := silence.Load(); n < 12 {
		t.Fatalf("OnSilence called %d times, want 12", n)
	}
}
