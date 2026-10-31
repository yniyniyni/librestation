package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"voice/internal/led/proto"
	"voice/internal/volume"
)

// rampStep is how long the amplifier stays on each 1% on its way to a new
// volume, so changes glide instead of jumping (15 -> 60 takes ~0.7 s).
const rampStep = 15 * time.Millisecond

// output turns the one volume into loudness: the gain file for aaudio_pipe is
// written at once, the amplifier is walked to the new volume 1% at a time by a
// long-running helper (`hwvol.sh -`, one value per stdin line).
type output struct {
	hwCmd    []string
	gainFile string

	mu     sync.Mutex
	target int
	force  bool // set the amplifier to target even if it seems there already
	wake   chan struct{}

	led  func(proto.Event) // optional; must not block
	prev int               // last volume passed to set; -1 = none yet
}

func newOutput(hwCmd []string, gainFile string) *output {
	return &output{hwCmd: hwCmd, gainFile: gainFile, target: -1, prev: -1, wake: make(chan struct{}, 1)}
}

// set is volume.Sync.Out: it must not block.
func (o *output) set(v int) {
	if o.gainFile != "" {
		g := strconv.FormatFloat(volume.Compensation(v), 'g', 8, 64)
		tmp := o.gainFile + ".tmp"
		err := os.WriteFile(tmp, []byte(g+"\n"), 0o644)
		if err == nil {
			err = os.Rename(tmp, o.gainFile)
		}
		if err != nil {
			log.Printf("gain: %v", err)
		}
	}
	o.mu.Lock()
	o.target = v
	dir := 0
	if o.prev >= 0 && v != o.prev {
		dir = 1
		if v < o.prev {
			dir = -1
		}
	}
	o.prev = v
	o.mu.Unlock()
	if o.led != nil {
		o.led(proto.Event{T: "volume", V: v, Dir: dir})
	}
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// refresh sets the amplifier to v again, for when something else (the audio
// HAL waking from standby) may have changed it behind our back.
func (o *output) refresh(v int) {
	o.mu.Lock()
	o.target, o.force = v, true
	o.mu.Unlock()
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

// wanted returns the target and whether it must be written even when the
// amplifier is believed to be there.
func (o *output) wanted() (int, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	f := o.force
	o.force = false
	return o.target, f
}

// run keeps the helper alive and ramps the amplifier toward the target.
func (o *output) run(ctx context.Context) {
	if len(o.hwCmd) == 0 {
		return
	}
	cur := -1 // unknown until the first value is set directly
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, o.hwCmd[0], o.hwCmd[1:]...)
		in, err := cmd.StdinPipe()
		if err == nil {
			var out io.ReadCloser
			if out, err = cmd.StdoutPipe(); err == nil {
				cmd.Stderr = cmd.Stdout
				if err = cmd.Start(); err == nil {
					go logLines(out)
					cur, err = o.ramp(ctx, in, cur)
					in.Close()
					cmd.Wait()
				}
			}
		}
		if ctx.Err() == nil {
			log.Printf("amplifier helper: %v; restarting", err)
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}
}

// ramp feeds the helper until ctx ends or a write fails; it returns the last
// value the helper was given.
func (o *output) ramp(ctx context.Context, in io.Writer, cur int) (int, error) {
	for {
		t, force := o.wanted()
		if force && t >= 0 {
			cur = -1 // unknown: write t directly
		}
		if t < 0 || t == cur {
			select {
			case <-ctx.Done():
				return cur, ctx.Err()
			case <-o.wake:
			}
			continue
		}
		next := t
		if cur >= 0 && t > cur {
			next = cur + 1
		} else if cur >= 0 {
			next = cur - 1
		}
		if _, err := fmt.Fprintf(in, "%d\n", next); err != nil {
			return cur, err
		}
		cur = next
		if cur != t {
			time.Sleep(rampStep)
		} else {
			log.Printf("amplifier volume %d", cur)
		}
	}
}

// logLines logs the helper's complaints; read-backs of good values are dropped.
func logLines(r io.Reader) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if line := sc.Text(); !strings.HasPrefix(line, "hw_master_volume=") {
			log.Printf("amplifier helper: %s", line)
		}
	}
}
