package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
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
	// liveLead is the Live audio held back before a reply's first write, so
	// network jitter between chunks does not starve the stream.
	liveLead = 200 * time.Millisecond
)

// errFlushed cancels the Play, PlayLive or FinishLive that Flush interrupts.
var errFlushed = errors.New("speaker: flushed")

// speaker plays replies through a resident aaudio_pipe. It runs without a
// gain file, so the loudness of a reply is only the amplifier's, the same as
// the user's volume.
type speaker struct {
	cmd    []string
	onWake func() // the output woke from idle: the HAL may have reset the amplifier

	now          func() time.Time
	sleep        func(ctx context.Context, d time.Duration) error
	writeTimeout time.Duration

	play    sync.Mutex // one Play at a time; guards the fields below
	proc    *speakerProc
	last    time.Time // when the last chunk was written; zero = stream not running
	lead    []float32 // Live audio held back before the reply's first write
	liveEnd time.Time // when the Live reply written so far ends playing; zero = none

	op       sync.Mutex              // guards cancelOp
	cancelOp context.CancelCauseFunc // the running call under s.play, for Flush; nil = none
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
	s.lead, s.liveEnd = nil, time.Time{}
}

// Flush drops all queued audio at once: the held-back Live lead-in and, by
// killing the helper, what is in the device. A write or wait in progress
// returns nil. The next Play or PlayLive starts a fresh helper.
func (s *speaker) Flush() {
	s.op.Lock()
	if s.cancelOp != nil {
		s.cancelOp(errFlushed)
	}
	s.op.Unlock()
	s.play.Lock()
	defer s.play.Unlock()
	s.kill()
}

// begin makes ctx cancellable by Flush for one call under s.play. The
// returned func ends the call and turns a flushed result into nil.
func (s *speaker) begin(ctx context.Context) (context.Context, func(*error)) {
	ctx, cancel := context.WithCancelCause(ctx)
	s.op.Lock()
	s.cancelOp = cancel
	s.op.Unlock()
	return ctx, func(err *error) {
		s.op.Lock()
		s.cancelOp = nil
		s.op.Unlock()
		if *err != nil && context.Cause(ctx) == errFlushed {
			*err = nil
		}
		cancel(nil)
	}
}

// open starts the helper if needed and reports a wake from idle. The caller
// holds s.play.
func (s *speaker) open() (*speakerProc, error) {
	p, err := s.start()
	if err != nil {
		return nil, err
	}
	if s.last.IsZero() || s.now().Sub(s.last) > speakerIdle {
		if s.onWake != nil {
			s.onWake()
		}
	}
	return p, nil
}

// Play writes pcm (mono, rate Hz) to the stream and returns once it has had
// time to play. Cancelling ctx stops writing at once; at most the device
// buffer keeps playing. If the helper dies the error is returned and the next
// Play restarts it.
func (s *speaker) Play(ctx context.Context, pcm []int16, rate int) (err error) {
	s.play.Lock()
	defer s.play.Unlock()
	ctx, done := s.begin(ctx)
	defer done(&err)
	if err := ctx.Err(); err != nil {
		return err
	}
	if rate <= 0 {
		return fmt.Errorf("speaker: bad rate %d", rate)
	}
	p, err := s.open()
	if err != nil {
		return err
	}
	frames := resample24k(pcm, rate)
	if len(frames) == 0 {
		return nil
	}
	start := s.now()
	if err := s.writeLocked(ctx, p, frames); err != nil {
		return err
	}
	wait := start.Add(framesDuration(frames) + speakerBuffer).Sub(s.now())
	if wait <= 0 {
		return nil
	}
	return s.sleep(ctx, wait)
}

// PlayLive appends one Live audio chunk without waiting for it to play. The
// first liveLead of a reply is held back and written in one go; after that
// each chunk is written at once. FinishLive ends the reply.
func (s *speaker) PlayLive(ctx context.Context, pcm []int16, rate int) (err error) {
	s.play.Lock()
	defer s.play.Unlock()
	ctx, done := s.begin(ctx)
	defer done(&err)
	if err := ctx.Err(); err != nil {
		return err
	}
	if rate <= 0 {
		return fmt.Errorf("speaker: bad rate %d", rate)
	}
	frames := resample24k(pcm, rate)
	if s.liveEnd.IsZero() {
		s.lead = append(s.lead, frames...)
		if framesDuration(s.lead) < liveLead {
			return nil
		}
		frames, s.lead = s.lead, nil
	}
	return s.writeLive(ctx, frames)
}

// FinishLive writes what is still held back and waits until the whole reply
// has played: from when its audio started, not from the last write.
func (s *speaker) FinishLive(ctx context.Context) (err error) {
	s.play.Lock()
	defer s.play.Unlock()
	ctx, done := s.begin(ctx)
	defer done(&err)
	frames := s.lead
	s.lead = nil
	if err := s.writeLive(ctx, frames); err != nil {
		return err
	}
	end := s.liveEnd
	s.liveEnd = time.Time{}
	if end.IsZero() {
		return nil
	}
	wait := end.Add(speakerBuffer).Sub(s.now())
	if wait <= 0 {
		return nil
	}
	return s.sleep(ctx, wait)
}

// writeLive writes frames of the current Live reply and moves its end: the
// audio plays after what was queued, or from now if the stream ran dry. A
// failed write ends the reply. The caller holds s.play.
func (s *speaker) writeLive(ctx context.Context, frames []float32) error {
	if len(frames) == 0 {
		return nil
	}
	p, err := s.open()
	if err == nil {
		t := s.now()
		if err = s.writeLocked(ctx, p, frames); err == nil {
			s.liveEnd = later(s.liveEnd, t).Add(framesDuration(frames))
			return nil
		}
	}
	s.lead, s.liveEnd = nil, time.Time{}
	return err
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func framesDuration(frames []float32) time.Duration {
	return time.Duration(len(frames)) * time.Second / speakerRate
}

func (s *speaker) writeLocked(ctx context.Context, p *speakerProc, frames []float32) error {
	if len(frames) == 0 {
		return nil
	}
	var guard struct {
		sync.Mutex
		done bool
	}
	arm := func() {
		guard.Lock()
		defer guard.Unlock()
		if ctx.Err() == nil {
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

	per := int(int64(speakerRate) * int64(speakerChunk) / int64(time.Second))
	buf := make([]byte, 0, per*8)
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
		arm()
		n, err := p.in.Write(buf)
		s.last = s.now()
		if err != nil {
			if context.Cause(ctx) == errFlushed {
				s.kill() // Flush kills it anyway; no frame to finish
				return ctx.Err()
			}
			if ctx.Err() != nil {
				finish()
				if rest := (8 - n%8) % 8; rest > 0 && n+rest <= len(buf) {
					p.in.SetWriteDeadline(time.Now().Add(time.Second))
					if _, writeErr := p.in.Write(buf[n : n+rest]); writeErr != nil {
						s.kill()
					}
				}
				return ctx.Err()
			}
			s.kill()
			return fmt.Errorf("speaker: %w", err)
		}
		if n != len(buf) {
			s.kill()
			return io.ErrShortWrite
		}
	}
	return nil
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
