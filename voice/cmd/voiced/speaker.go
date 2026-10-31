package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"sync"
	"time"
)

const (
	speakerRate = 24000

	// speakerChunk is how much audio goes into one write, so a cancel is seen
	// within that long.
	speakerChunk = 20 * time.Millisecond
	// speakerIdle is when aaudio_pipe pauses its stream for lack of data.
	speakerIdle = 1500 * time.Millisecond
	// speakerBuffer is the audio still queued in the device after the last
	// write.
	speakerBuffer = 340 * time.Millisecond
	// speakerWriteTimeout bounds one chunk's write: a helper that stops
	// reading is killed instead of blocking Play for good.
	speakerWriteTimeout = 2 * time.Second
)

// speaker plays replies through a resident aaudio_pipe. It runs without a
// gain file, so the loudness of a reply is only the amplifier's, the same as
// the user's volume.
type speaker struct {
	cmd    []string
	onWake func() // the output woke from idle: the HAL may have reset the amplifier

	now          func() time.Time
	sleep        func(ctx context.Context, d time.Duration) error
	writeTimeout time.Duration

	play sync.Mutex // one Play at a time; guards the fields below
	proc *speakerProc
	last time.Time // when the last chunk was written; zero = stream not running
}

// speakerProc is one run of the helper.
type speakerProc struct {
	cmd  *exec.Cmd
	in   *os.File
	dead chan struct{}
}

func newSpeaker(cmd []string, onWake func()) *speaker {
	return &speaker{cmd: cmd, onWake: onWake, now: time.Now, sleep: sleepCtx, writeTimeout: speakerWriteTimeout}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// start launches the helper if it is not running.
func (s *speaker) start() (*speakerProc, error) {
	if p := s.proc; p != nil {
		select {
		case <-p.dead:
		default:
			return p, nil
		}
	}
	s.proc, s.last = nil, time.Time{}
	if len(s.cmd) == 0 {
		return nil, errors.New("speaker: no command")
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(s.cmd[0], s.cmd[1:]...)
	cmd.Stdin = r
	out, err := cmd.StdoutPipe()
	if err == nil {
		cmd.Stderr = cmd.Stdout
		err = cmd.Start()
	}
	r.Close()
	if err != nil {
		w.Close()
		return nil, err
	}
	p := &speakerProc{cmd: cmd, in: w, dead: make(chan struct{})}
	go func() {
		logLines("speaker", out)
		err := cmd.Wait()
		w.Close()
		close(p.dead)
		log.Printf("speaker helper exited: %v", err)
	}()
	s.proc = p
	return p, nil
}

// Close stops the helper.
func (s *speaker) Close() {
	s.play.Lock()
	defer s.play.Unlock()
	s.kill()
}

// kill stops the helper and forgets it, so the next Play starts a fresh one on
// a clean stream. The caller holds s.play.
func (s *speaker) kill() {
	if p := s.proc; p != nil {
		p.in.Close()
		p.cmd.Process.Kill()
	}
	s.proc, s.last = nil, time.Time{}
}

// Play writes pcm (mono, rate Hz) to the stream and returns once it has had
// time to play. Cancelling ctx stops writing at once; at most the device
// buffer keeps playing. If the helper dies the error is returned and the next
// Play restarts it.
func (s *speaker) Play(ctx context.Context, pcm []int16, rate int) error {
	s.play.Lock()
	defer s.play.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if rate <= 0 {
		return fmt.Errorf("speaker: bad rate %d", rate)
	}
	p, err := s.start()
	if err != nil {
		return err
	}
	if s.last.IsZero() || s.now().Sub(s.last) > speakerIdle {
		if s.onWake != nil {
			s.onWake()
		}
	}

	// A blocked write (aaudio_pipe backed up) is released by the deadline:
	// each chunk gets writeTimeout, and a cancel moves it to now.
	// The callback may only touch the deadline while this Play is still
	// writing; once guard.done is set a late callback does nothing.
	var guard struct {
		sync.Mutex
		done bool
	}
	arm := func() {
		guard.Lock()
		defer guard.Unlock()
		if ctx.Err() == nil { // else the callback has set it, or will
			p.in.SetWriteDeadline(time.Now().Add(s.writeTimeout))
		}
	}
	finish := func() {
		guard.Lock()
		guard.done = true
		guard.Unlock()
	}
	stop := context.AfterFunc(ctx, func() {
		guard.Lock()
		defer guard.Unlock()
		if !guard.done {
			p.in.SetWriteDeadline(time.Now())
		}
	})
	defer func() { finish(); stop() }()

	frames := resample24k(pcm, rate)
	per := int(int64(speakerRate) * int64(speakerChunk) / int64(time.Second))
	buf := make([]byte, 0, per*8)
	var start time.Time
	for off := 0; off < len(frames); off += per {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk := frames[off:min(off+per, len(frames))]
		buf = buf[:0]
		for _, v := range chunk {
			buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(v))
			buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(v))
		}
		if start.IsZero() {
			start = s.now()
		}
		arm()
		n, err := p.in.Write(buf)
		s.last = s.now()
		if err != nil {
			if ctx.Err() != nil {
				// Finish the frame cut by the deadline so the stream stays
				// aligned; if that fails, restart the helper on a clean stream.
				finish()
				if rest := (8 - n%8) % 8; rest > 0 {
					p.in.SetWriteDeadline(time.Now().Add(time.Second))
					if _, err := p.in.Write(buf[n : n+rest]); err != nil {
						s.kill()
					}
				}
				return ctx.Err()
			}
			s.kill()
			return fmt.Errorf("speaker: %w", err)
		}
	}
	if start.IsZero() {
		return nil
	}
	played := time.Duration(len(frames)) * time.Second / speakerRate
	wait := start.Add(played + speakerBuffer).Sub(s.now())
	if wait <= 0 {
		return nil
	}
	return s.sleep(ctx, wait)
}

// resample24k converts mono int16 at rate Hz to float32 at speakerRate by
// linear interpolation.
func resample24k(pcm []int16, rate int) []float32 {
	if len(pcm) == 0 {
		return nil
	}
	n := int(int64(len(pcm)) * speakerRate / int64(rate))
	out := make([]float32, n)
	for i := range out {
		pos := float64(i) * float64(rate) / speakerRate
		j := int(pos)
		frac := float32(pos - float64(j))
		a := float32(pcm[min(j, len(pcm)-1)])
		b := float32(pcm[min(j+1, len(pcm)-1)])
		out[i] = (a + (b-a)*frac) / 32768
	}
	return out
}
