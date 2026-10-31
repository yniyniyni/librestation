package assistant

import (
	"context"
	"errors"
	"log"
	"sync"

	"voice/internal/geminilive"
)

// liveConn is a connection with its sender and receiver goroutines.
type liveConn struct {
	c       LiveSession
	msgs    chan liveMsg
	sendErr chan error
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

type liveMsg struct {
	m   geminilive.Message
	err error
}

func (s *Session) openConn(ctx context.Context, c LiveSession) *liveConn {
	ctx, cancel := context.WithCancel(ctx)
	lc := &liveConn{c: c, msgs: make(chan liveMsg), sendErr: make(chan error, 1), cancel: cancel}
	lc.wg.Add(2)
	go func() {
		defer lc.wg.Done()
		for {
			m, err := c.Receive(ctx)
			select {
			case lc.msgs <- liveMsg{m, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		defer lc.wg.Done()
		if err := s.liveSend(ctx, c); err != nil && ctx.Err() == nil {
			lc.sendErr <- err
		}
	}()
	return lc
}

// close stops the goroutines: Close lets go of a blocked Receive.
func (lc *liveConn) close() {
	lc.cancel()
	lc.c.Close()
	lc.wg.Wait()
}

// liveSend streams the queued LiveAudio in liveChunk pieces until ctx ends.
func (s *Session) liveSend(ctx context.Context, c LiveSession) error {
	var buf, spare []int16
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.lnote:
		}
		s.amu.Lock()
		blk := s.lq
		s.lq = spare[:0]
		s.amu.Unlock()
		buf, spare = append(buf, blk...), blk
		n := 0
		for ; len(buf)-n >= liveChunk; n += liveChunk {
			if err := c.StreamAudio(ctx, buf[n:n+liveChunk]); err != nil {
				return err
			}
		}
		buf = buf[:copy(buf, buf[n:])]
	}
}

// liveOut plays the reply on its own goroutine, so a barge-in is acted on
// while a chunk is being written. flush starts a new epoch: what was queued
// before it is dropped.
type liveOut struct {
	lp     livePlayer
	note   chan struct{}
	played chan livePlayed // FinishLive results

	mu    sync.Mutex
	q     []liveClip
	epoch int
}

type liveClip struct {
	pcm   []int16
	end   bool // FinishLive
	epoch int
}

type livePlayed struct {
	epoch int
	err   error
}

func newLiveOut(lp livePlayer) *liveOut {
	return &liveOut{lp: lp, note: make(chan struct{}, 1), played: make(chan livePlayed)}
}

func (o *liveOut) play(pcm []int16) { o.push(liveClip{pcm: pcm}) }

// finish ends the reply; its result comes on played with the epoch returned.
func (o *liveOut) finish() int { return o.push(liveClip{end: true}) }

func (o *liveOut) push(c liveClip) int {
	o.mu.Lock()
	c.epoch = o.epoch
	o.q = append(o.q, c)
	o.mu.Unlock()
	poke(o.note)
	return c.epoch
}

func (o *liveOut) flush() {
	o.mu.Lock()
	o.epoch++
	o.q = nil
	o.mu.Unlock()
	o.lp.Flush()
}

func (o *liveOut) run(ctx context.Context) {
	for {
		o.mu.Lock()
		if len(o.q) == 0 {
			o.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-o.note:
			}
			continue
		}
		c := o.q[0]
		o.q = o.q[1:]
		o.mu.Unlock()
		var err error
		if c.end {
			err = o.lp.FinishLive(ctx)
		} else {
			err = o.lp.PlayLive(ctx, c.pcm, liveRate)
		}
		o.mu.Lock()
		stale := c.epoch != o.epoch
		o.mu.Unlock()
		switch {
		case stale:
			if err == nil {
				// It began after the flush, on a fresh helper: drop it too.
				o.lp.Flush()
			}
		case c.end:
			select {
			case o.played <- livePlayed{c.epoch, err}:
			case <-ctx.Done():
				return
			}
		case err != nil && !errors.Is(err, ErrFlushed) && ctx.Err() == nil:
			log.Printf("assistant: Live play: %v", err)
		}
	}
}

func poke(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}
