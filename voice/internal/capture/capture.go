// Package capture runs the mic helper process and frames its stdout into
// 16-channel int16 blocks, restarting it on failure and watching for silence.
// While the mic is muted by the station's switch the helper does not run.
package capture

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"time"
)

const (
	Channels    = 16
	Rate        = 48000
	BlockFrames = 960

	// silenceBlocks is 5 s of audio: 5*48000/960.
	silenceBlocks = 5 * Rate / BlockFrames
)

var (
	errSilence = errors.New("capture: sustained silence")
	errMuted   = errors.New("capture: muted")
)

// Backoff bounds between helper restarts and the mute switch's poll period;
// variables so tests can shrink them.
var (
	backoffMin = 1 * time.Second
	backoffMax = 30 * time.Second
	mutePoll   = 500 * time.Millisecond
)

// ReadBlocks reads interleaved little-endian int16 data from r and calls fn
// with exactly BlockFrames*Channels samples per call. The slice is reused
// between calls. A partial trailing block yields io.EOF.
func ReadBlocks(r io.Reader, fn func(block []int16) error) error {
	raw := make([]byte, BlockFrames*Channels*2)
	block := make([]int16, BlockFrames*Channels)
	for {
		n, err := io.ReadFull(r, raw)
		if err != nil {
			if err == io.ErrUnexpectedEOF || (err == io.EOF && n == 0) {
				return io.EOF
			}
			return err
		}
		for i := range block {
			block[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
		}
		if err := fn(block); err != nil {
			return err
		}
	}
}

type silenceDetector struct {
	limit, count int
	heard        bool // saw a non-zero sample
}

func newSilenceDetector(limit int) *silenceDetector { return &silenceDetector{limit: limit} }

// feed returns true once per `limit` consecutive all-zero blocks.
func (d *silenceDetector) feed(block []int16) bool {
	for _, s := range block {
		if s != 0 {
			d.count, d.heard = 0, true
			return false
		}
	}
	d.count++
	if d.count >= d.limit {
		d.count = 0
		return true
	}
	return false
}

// Runner starts the helper process and delivers its audio blocks.
type Runner struct {
	Cmd       []string
	Env       []string      // extra environment entries (appended to the current one); nil = inherit only
	OnBlock   func([]int16) // slice is reused after return; called on the read goroutine
	OnSilence func()
	// Muted reads the station's mic switch; nil = no switch. A muted mic gives
	// zeros faster than real time, so reading them costs more than a live mic.
	Muted   func() bool
	OnMuted func() // called once each time the helper stops for the switch
}

// Run runs the helper until ctx is cancelled, restarting it with exponential
// backoff (1 s to 30 s) whenever it exits or stays silent. While Muted holds
// the helper is not running; it starts as soon as the switch is back. It
// returns ctx.Err() on cancel.
func (r *Runner) Run(ctx context.Context) error {
	if len(r.Cmd) == 0 {
		return errors.New("capture: empty Cmd")
	}
	backoff := backoffMin
	for {
		if err := r.waitUnmuted(ctx); err != nil {
			return err
		}
		started := time.Now()
		got, heard, err := r.runOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errMuted) {
			continue
		}
		if errors.Is(err, errSilence) {
			// Mic was held by another app: reopen it, but the longer it stays
			// silent the less often, and promptly again once it has worked.
			if heard {
				backoff = backoffMin
			}
			log.Printf("capture: silence detected, restarting helper in %s", backoff)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			if backoff *= 2; backoff > backoffMax {
				backoff = backoffMax
			}
			continue
		}
		log.Printf("capture: helper exited after %s (%d blocks): %v; restart in %s",
			time.Since(started).Round(time.Millisecond), got, err, backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if got > 0 {
			backoff = backoffMin
		} else if backoff *= 2; backoff > backoffMax {
			backoff = backoffMax
		}
	}
}

// waitUnmuted returns once the switch is off, or ctx.Err() on cancel.
func (r *Runner) waitUnmuted(ctx context.Context) error {
	if r.Muted == nil || !r.Muted() {
		return nil
	}
	log.Printf("capture: mic muted by the switch, helper stopped")
	if r.OnMuted != nil {
		r.OnMuted()
	}
	t := time.NewTicker(mutePoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		if !r.Muted() {
			log.Printf("capture: mic unmuted, starting helper")
			return nil
		}
	}
}

// runOnce runs the helper once and returns how many blocks it gave, whether
// any of them had sound, and why it ended.
func (r *Runner) runOnce(ctx context.Context) (int, bool, error) {
	cmd := exec.CommandContext(ctx, r.Cmd[0], r.Cmd[1:]...)
	if len(r.Env) > 0 {
		cmd.Env = append(cmd.Environ(), r.Env...)
	}
	cmd.WaitDelay = 2 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, false, err
	}
	lw := &lineLogger{}
	cmd.Stderr = lw // exec copies it fully before Wait returns
	if err := cmd.Start(); err != nil {
		return 0, false, err
	}

	det := newSilenceDetector(silenceBlocks)
	blocks := 0
	rerr := ReadBlocks(stdout, func(b []int16) error {
		blocks++
		if r.OnBlock != nil {
			r.OnBlock(b)
		}
		if det.feed(b) {
			if r.Muted != nil && r.Muted() {
				return errMuted
			}
			if r.OnSilence != nil {
				r.OnSilence()
			}
			return errSilence
		}
		return ctx.Err()
	})
	if cmd.Process != nil {
		_ = cmd.Process.Kill() // no-op if already exited
	}
	werr := cmd.Wait()
	lw.flush()
	if rerr == io.EOF && werr != nil {
		rerr = fmt.Errorf("eof; %w", werr)
	}
	return blocks, det.heard, rerr
}

// lineLogger forwards helper stderr to the log, line by line.
type lineLogger struct{ buf []byte }

func (l *lineLogger) Write(p []byte) (int, error) {
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		log.Printf("capture helper: %s", bytes.TrimRight(l.buf[:i], "\r"))
		l.buf = l.buf[i+1:]
	}
	return len(p), nil
}

func (l *lineLogger) flush() {
	if len(l.buf) > 0 {
		log.Printf("capture helper: %s", l.buf)
		l.buf = nil
	}
}
