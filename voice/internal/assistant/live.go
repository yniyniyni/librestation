package assistant

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"voice/internal/agent"
	"voice/internal/geminilive"
)

// LiveSession is one Gemini Live connection; *geminilive.Client implements
// it. Its writes are serialised, so StreamAudio and Respond may run at once.
type LiveSession interface {
	StreamAudio(context.Context, []int16) error
	Receive(context.Context) (geminilive.Message, error)
	Respond(context.Context, []geminilive.FunctionResponse) error
	Close()
}

// livePlayer streams Live PCM without waiting after every small audio chunk.
// The existing Player interface remains blocking for regular TTS replies.
// Flush drops all queued audio at once (barge-in); the calls it interrupts, or
// that race with it, return ErrFlushed, and the rest of that turn should be
// dropped.
type livePlayer interface {
	PlayLive(context.Context, []int16, int) error
	FinishLive(context.Context) error
	Flush()
}

// ErrFlushed is returned by a livePlayer call cut short by Flush.
var ErrFlushed = errors.New("assistant: playback flushed")

const (
	liveRate = 24000 // Live's reply audio
	// liveChunk is one StreamAudio call: 100 ms of channel 0.
	liveChunk = sampleRate / 10
	// maxLiveQueued bounds the channel 0 audio waiting to be sent; the
	// oldest goes first.
	maxLiveQueued = 4 * sampleRate
	// liveStall ends the conversation when the server sends nothing for this
	// long while a reply is awaited.
	liveStall = 15 * time.Second
	// toolTimeout bounds one tool call.
	toolTimeout = 15 * time.Second

	// bargeInDBFS is the LiveAudio level (channel 0 ×LiveGain, LiveGain = 2)
	// that, held for bargeInFor while the reply plays, cuts the reply off.
	// Measured on the station (ledger, Task 1; channel 0 ×1): the assistant's
	// own voice leaves a residual of p99 ≈ −39 dBFS, the user's speech at
	// 1–3 m peaks at p99 ≈ −22…−24 dBFS. −31 dBFS ×1 sits between, +6 dB of
	// gain makes it −25.
	bargeInDBFS = -25.0
	bargeInFor  = 150 * time.Millisecond
)

const LivePrompt = `Ты голосовой помощник умной колонки. Отвечай по-русски коротко и естественно. Используй объявленные инструменты для музыки, громкости, погоды и времени. Не утверждай, что действие выполнено без успешного результата. Если пользователь просит закончить разговор, вызови end_conversation.`

func LiveDeclarations(tools []agent.Tool) []geminilive.Declaration {
	out := make([]geminilive.Declaration, 0, len(tools)+1)
	for _, t := range tools {
		d := t.Decl()
		out = append(out, geminilive.Declaration{Name: d.Name, Description: d.Description, Parameters: d.Params, Behavior: "BLOCKING"})
	}
	out = append(out, geminilive.Declaration{Name: "end_conversation", Description: "Закончить разговор по просьбе пользователя.", Parameters: map[string]any{"type": "OBJECT", "properties": map[string]any{}}, Behavior: "BLOCKING"})
	return out
}

func pcm24(blob *geminilive.Blob) ([]int16, error) {
	if blob == nil || len(blob.Data)%2 != 0 {
		return nil, errors.New("assistant: invalid Live PCM")
	}
	if blob.MimeType != "audio/pcm" && blob.MimeType != "audio/pcm;rate=24000" {
		return nil, fmt.Errorf("assistant: unsupported Live audio %q", blob.MimeType)
	}
	out := make([]int16, len(blob.Data)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(blob.Data[i*2:]))
	}
	return out, nil
}

// voiceEvent is what the voice watch saw since it was last read.
type voiceEvent uint8

const (
	evStart voiceEvent = 1 << iota // the user started speaking
	evEnd                          // and stopped
	evLoud                         // loud enough for long enough to barge in
)

// voiceWatch is the local VAD on the LiveAudio stream: the endpointer's
// start and end rules against a noise floor fixed when it is armed, and the
// absolute barge-in level. It is fed every block, under amu.
type voiceWatch struct {
	lv    levels
	seen  int // lv frames already judged
	floor float64

	run, quiet, loud int
	speaking         bool
	ev               voiceEvent
}

// arm takes a new noise floor from the recent audio and forgets the past.
func (w *voiceWatch) arm() {
	w.floor = w.lv.floor()
	w.run, w.quiet, w.loud, w.speaking, w.ev = 0, 0, 0, false, 0
}

// push takes in pcm and reports whether a new event was noted.
func (w *voiceWatch) push(pcm []int16) bool {
	w.lv.push(pcm)
	before := w.ev
	w.seen = max(w.seen, w.lv.frames-len(w.lv.ring))
	for ; w.seen < w.lv.frames; w.seen++ {
		w.frame(w.lv.ring[w.seen%len(w.lv.ring)])
	}
	return w.ev != before
}

func (w *voiceWatch) frame(db float64) {
	if db < bargeInDBFS {
		w.loud = 0
	} else if w.loud++; w.loud == int(bargeInFor/frameDur) {
		w.ev |= evLoud
	}
	speech := db >= w.floor+speechAbove
	if !w.speaking {
		if !speech {
			w.run = 0
		} else if w.run++; w.run >= startFrames {
			w.speaking, w.run, w.quiet = true, 0, 0
			w.ev |= evStart
		}
		return
	}
	if speech {
		w.quiet = 0
	} else if w.quiet++; w.quiet >= int(endQuiet/frameDur) {
		w.speaking = false
		w.ev |= evEnd
	}
}

// liveOn makes LiveAudio queue for sending, starting with the pre-roll when
// seed is set, and arms the voice watch.
func (s *Session) liveOn(seed bool) {
	s.amu.Lock()
	s.live, s.lq = true, s.lq[:0]
	if seed {
		s.lq = s.lraw.appendTo(s.lq)
	}
	s.watch.arm()
	s.amu.Unlock()
	poke(s.lnote)
}

func (s *Session) liveOff() {
	s.amu.Lock()
	s.live, s.lq = false, s.lq[:0]
	s.amu.Unlock()
}

// rearm re-arms the voice watch, for a floor without the reply in it.
func (s *Session) rearm() {
	s.amu.Lock()
	s.watch.arm()
	s.amu.Unlock()
}

func poke(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

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

// liveTurn is one exchange: the user's words, the tools run and the reply.
type liveTurn struct {
	heard, reply string
	from         time.Time     // when the wait for the reply began
	first        time.Duration // from then to the first audio; -1 = none

	mu    sync.Mutex
	tools []toolResult // appended by the tool goroutines
}

func (t *liveTurn) add(r toolResult) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tools = append(t.tools, r)
}

func (t *liveTurn) summary() turnSummary {
	t.mu.Lock()
	defer t.mu.Unlock()
	return turnSummary{Tools: append([]toolResult(nil), t.tools...), Reply: t.reply}
}

// liveRun is one conversation over Live. Its fields belong to the run's
// goroutine; the connection, the player and the tools have their own.
type liveRun struct {
	s           *Session
	gen         int
	ctx         context.Context // cancelled by Hush or a new Wake
	tctx        context.Context // the tools'; cancelled when the run ends
	lp          livePlayer
	onNoRequest func()
	byName      map[string]agent.Tool

	conn   *liveConn
	out    *liveOut
	handle string // the latest resumption handle
	goAway bool   // the server is ending the connection: resume after the turn

	state     string
	user      bool      // the voice watch hears the user
	turn      *liveTurn // nil between turns
	turns     int
	drop      bool // a barge-in cut the reply: drop the rest of its audio
	finishing bool // the reply's FinishLive is under way
	finishOf  int  // its epoch
	window    <-chan time.Time
	wait      time.Duration // the window's length
	stall     <-chan time.Time
	tools     int // tool batches running
	cancels   map[string]context.CancelFunc
	toolDone  chan toolBatch
	twg       sync.WaitGroup

	err error  // ends the conversation with a stock phrase
	why string // why it ended, for the log
}

type toolBatch struct {
	ids []string
	err error // from Respond
}

// runLive holds the conversation over one Live session. It reports false,
// having done nothing else, when Live does not connect: the conversation then
// goes to the agent.
func (s *Session) runLive(ctx context.Context, gen int, lp livePlayer, seed bool, onNoRequest func()) bool {
	s.liveOn(seed) // audio queues while the connection opens
	started := time.Now()
	c, err := s.d.Live(ctx, "")
	if err != nil {
		s.liveOff()
		if ctx.Err() != nil {
			s.idle(gen)
			return true
		}
		log.Printf("assistant: Live unavailable, using the agent: %v", err)
		return false
	}
	log.Printf("assistant: Live session open in %v", time.Since(started).Round(time.Millisecond))
	r := &liveRun{s: s, gen: gen, ctx: ctx, lp: lp, onNoRequest: onNoRequest,
		byName: map[string]agent.Tool{}, cancels: map[string]context.CancelFunc{}, toolDone: make(chan toolBatch)}
	for _, t := range s.d.Agent.Tools {
		r.byName[t.Decl().Name] = t
	}
	r.run(c)
	return true
}

func (r *liveRun) run(c LiveSession) {
	octx, stopOut := context.WithCancel(r.ctx)
	r.out = newLiveOut(r.lp)
	var owg sync.WaitGroup
	owg.Add(1)
	go func() {
		defer owg.Done()
		r.out.run(octx)
	}()
	tctx, stopTools := context.WithCancel(r.ctx)
	r.tctx = tctx
	r.conn = r.s.openConn(r.ctx, c)

	r.listen(startLimit)
	r.loop()

	if r.conn != nil {
		r.conn.close()
	}
	stopTools()
	r.twg.Wait()
	r.s.liveOff()
	cut := r.err != nil || r.ctx.Err() != nil || r.state == "speaking" || r.finishing
	stopOut()
	owg.Wait()
	if cut {
		r.lp.Flush()
	}
	if r.turn != nil {
		r.endTurn(r.why)
	}
	log.Printf("assistant: Live session closed: %s", r.why)
	if r.err != nil && r.ctx.Err() == nil {
		r.s.liveError(r.gen, r.err)
		r.s.fail(r.ctx, r.gen, r.err)
		return
	}
	r.s.idle(r.gen)
}

func (r *liveRun) loop() {
	for {
		ok := true
		select {
		case <-r.ctx.Done():
			r.why = "cancelled"
			return
		case m := <-r.conn.msgs:
			ok = r.message(m)
		case err := <-r.conn.sendErr:
			ok = r.lost(err)
		case <-r.s.vnote:
			r.voice()
		case p := <-r.out.played:
			ok = r.played(p)
		case b := <-r.toolDone:
			ok = r.toolsDone(b)
		case <-r.window:
			r.why = "silence"
			if r.turns == 0 && r.onNoRequest != nil && r.s.current(r.gen) {
				r.onNoRequest()
			}
			return
		case <-r.stall:
			r.err = fmt.Errorf("assistant: Live sent nothing for %v: %w", liveStall, context.DeadlineExceeded)
			r.why = "no answer"
			return
		}
		if !ok {
			return
		}
	}
}

// to moves the conversation to state and shows it on the ring.
func (r *liveRun) to(state string) {
	if r.state != state {
		r.state = state
		r.s.set(r.gen, state, state)
	}
}

// waiting is true while the conversation waits for the user.
func (r *liveRun) waiting() bool { return r.state == "listening" || r.state == "followup" }

// listen waits for the user; wait of silence ends the conversation. Called
// again, it restarts the window.
func (r *liveRun) listen(wait time.Duration) {
	r.wait, r.window, r.stall = wait, r.s.after(wait), nil
	r.to("listening")
}

// followup waits wait for the user after a reply has played.
func (r *liveRun) followup(wait time.Duration) {
	r.s.rearm()
	r.wait, r.window, r.stall = wait, r.s.after(wait), nil
	r.to("followup")
}

// think waits for the reply; the server must keep talking meanwhile.
func (r *liveRun) think() {
	r.window = nil
	if r.tools == 0 {
		r.stall = r.s.after(liveStall)
	}
	r.begin().from = time.Now()
	r.to("thinking")
}

// begin returns the turn, starting one if none is open.
func (r *liveRun) begin() *liveTurn {
	if r.turn == nil {
		r.turn = &liveTurn{from: time.Now(), first: -1}
		r.turns++
		r.s.recordLive(r.gen, r.turn)
	}
	return r.turn
}

// endTurn logs the turn, how it closed, and forgets it.
func (r *liveRun) endTurn(close string) {
	t := r.turn
	r.turn = nil
	tools := "none"
	if sum := t.summary(); len(sum.Tools) > 0 {
		var b strings.Builder
		for i, x := range sum.Tools {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(x.Name)
			if !x.OK {
				b.WriteString(" (error)")
			}
		}
		tools = b.String()
	}
	first := "-"
	if t.first >= 0 {
		first = fmt.Sprintf("%d ms", t.first.Milliseconds())
	}
	log.Printf("assistant: Live turn: heard %q, tools %s, reply %q, first audio %s, close %s",
		strings.TrimSpace(t.heard), tools, strings.TrimSpace(t.reply), first, close)
}

func (r *liveRun) message(lm liveMsg) bool {
	if lm.err != nil {
		return r.lost(lm.err)
	}
	if r.stall != nil {
		r.stall = r.s.after(liveStall)
	}
	m := lm.m
	if u := m.SessionResumptionUpdate; u != nil && u.Resumable && u.NewHandle != "" {
		r.handle = u.NewHandle
	}
	if c := m.ToolCallCancellation; c != nil {
		for _, id := range c.IDs {
			if cancel := r.cancels[id]; cancel != nil {
				cancel()
			}
		}
	}
	if m.ToolCall != nil {
		r.call(m.ToolCall.FunctionCalls)
	}
	if c := m.ServerContent; c != nil {
		if c.Interrupted {
			r.interrupted()
		}
		if tr := c.InputTranscription; tr != nil && tr.Text != "" {
			t := r.begin()
			t.heard += tr.Text
			r.s.recordLive(r.gen, t)
			if r.waiting() {
				if r.user {
					r.listen(r.wait)
				} else {
					r.think()
				}
			}
		}
		if c.ModelTurn != nil {
			for _, p := range c.ModelTurn.Parts {
				if p.InlineData == nil {
					continue
				}
				pcm, err := pcm24(p.InlineData)
				if err != nil {
					r.err, r.why = err, "bad audio"
					return false
				}
				r.audio(pcm)
			}
		}
		if tr := c.OutputTranscription; tr != nil && tr.Text != "" && !r.drop {
			t := r.begin()
			t.reply += tr.Text
			r.s.recordLive(r.gen, t)
		}
		if c.TurnComplete {
			r.turnComplete()
		}
	}
	if m.GoAway != nil {
		r.goAway = true
		if r.turn == nil && !r.finishing {
			return r.resume()
		}
	}
	return true
}

func (r *liveRun) audio(pcm []int16) {
	if r.drop {
		return
	}
	t := r.begin()
	if t.first < 0 {
		t.first = time.Since(t.from)
	}
	if r.state != "speaking" {
		r.window = nil
		if r.stall == nil && r.tools == 0 {
			r.stall = r.s.after(liveStall)
		}
		r.to("speaking")
	}
	r.out.play(pcm)
}

func (r *liveRun) turnComplete() {
	if r.drop {
		r.drop = false // the end of the reply a barge-in cut
		return
	}
	if r.turn == nil {
		return
	}
	r.stall = nil
	r.finishing, r.finishOf = true, r.out.finish()
}

// played closes the turn once its reply has played, by the closing rules.
func (r *liveRun) played(p livePlayed) bool {
	if !r.finishing || p.epoch != r.finishOf {
		return true
	}
	r.finishing = false
	if p.err != nil && !errors.Is(p.err, ErrFlushed) {
		log.Printf("assistant: Live play: %v", p.err)
	}
	now, wait := closeAfter(r.turn.summary())
	if now {
		r.endTurn("now")
		r.why = "turn done"
		return false
	}
	r.endTurn(fmt.Sprintf("after %v of silence", wait))
	if r.goAway && !r.resume() {
		return false
	}
	r.followup(wait)
	return true
}

// bargeIn cuts the reply off: the user spoke over it.
func (r *liveRun) bargeIn() {
	r.out.flush()
	r.drop = !r.finishing // the server may still be sending it
	r.finishing = false
	if r.turn != nil {
		r.endTurn("barge-in")
	}
	r.listen(dialogWindow)
}

// interrupted is the server's barge-in: it stopped the reply itself.
func (r *liveRun) interrupted() {
	if r.state == "speaking" || r.finishing {
		r.out.flush()
	}
	r.drop, r.finishing = false, false
	if r.turn != nil {
		r.endTurn("interrupted")
	}
	r.listen(dialogWindow)
}

// voice acts on the voice watch's events.
func (r *liveRun) voice() {
	r.s.amu.Lock()
	ev := r.s.watch.ev
	r.user = r.s.watch.speaking
	r.s.watch.ev = 0
	r.s.amu.Unlock()
	if ev&evLoud != 0 && r.state == "speaking" {
		r.bargeIn()
	}
	if ev&evStart != 0 && r.waiting() {
		r.listen(r.wait) // speech resets the window
	}
	if ev&evEnd != 0 && r.waiting() {
		if r.turn != nil && r.turn.heard != "" {
			r.think()
		} else {
			r.listen(r.wait)
		}
	}
}

// lost handles a failed connection: resumed if the server announced the end,
// a failure otherwise.
func (r *liveRun) lost(err error) bool {
	if r.ctx.Err() != nil {
		r.why = "cancelled"
		return false
	}
	if !r.goAway {
		r.err, r.why = err, "connection lost"
		return false
	}
	if r.turn != nil {
		r.endTurn("connection ended")
	}
	r.drop, r.finishing = false, false
	if !r.resume() {
		return false
	}
	r.listen(dialogWindow)
	return true
}

// resume moves the conversation to a new connection with the handle.
func (r *liveRun) resume() bool {
	r.goAway = false
	r.conn.close()
	r.conn = nil
	if r.handle == "" {
		r.why = "server ended the session, no handle to resume"
		return false
	}
	c, err := r.s.d.Live(r.ctx, r.handle)
	if err != nil {
		r.why = "resume failed: " + err.Error()
		return false
	}
	r.conn = r.s.openConn(r.ctx, c)
	log.Printf("assistant: Live session resumed")
	return true
}

// call runs a batch of tool calls on its own goroutine and answers them.
func (r *liveRun) call(calls []geminilive.FunctionCall) {
	t := r.begin()
	r.tools++
	r.stall = nil
	if r.waiting() {
		r.think()
	}
	ctxs := make([]context.Context, len(calls))
	stops := make([]context.CancelFunc, len(calls))
	ids := make([]string, len(calls))
	for i, c := range calls {
		ctxs[i], stops[i] = context.WithTimeout(r.tctx, toolTimeout)
		r.cancels[c.ID], ids[i] = stops[i], c.ID
	}
	conn := r.conn.c
	r.twg.Add(1)
	go func() {
		defer r.twg.Done()
		var resps []geminilive.FunctionResponse
		for i, c := range calls {
			resp, ok := r.runTool(ctxs[i], c)
			cancelled := errors.Is(ctxs[i].Err(), context.Canceled)
			stops[i]()
			if cancelled {
				continue // the server or the session no longer wants it
			}
			t.add(toolResult{Name: c.Name, OK: ok})
			resps = append(resps, geminilive.FunctionResponse{ID: c.ID, Name: c.Name, Response: resp})
		}
		var err error
		if len(resps) > 0 {
			err = conn.Respond(r.tctx, resps)
		}
		select {
		case r.toolDone <- toolBatch{ids, err}:
		case <-r.tctx.Done():
		}
	}()
}

func (r *liveRun) runTool(ctx context.Context, c geminilive.FunctionCall) (map[string]any, bool) {
	if c.Name == "end_conversation" {
		return map[string]any{"ok": true}, true
	}
	t := r.byName[c.Name]
	if t == nil {
		return map[string]any{"error": "unknown tool " + c.Name}, false
	}
	args := map[string]any{}
	if len(c.Args) > 0 && string(c.Args) != "null" {
		if err := json.Unmarshal(c.Args, &args); err != nil {
			return map[string]any{"error": "invalid arguments"}, false
		}
	}
	resp, err := t.Run(ctx, args)
	if err != nil {
		return map[string]any{"error": err.Error()}, false
	}
	return resp, true
}

func (r *liveRun) toolsDone(b toolBatch) bool {
	r.tools--
	for _, id := range b.ids {
		delete(r.cancels, id)
	}
	if b.err != nil {
		return r.lost(b.err)
	}
	if r.tools == 0 && !r.finishing && (r.state == "thinking" || r.state == "speaking") {
		r.stall = r.s.after(liveStall)
	}
	return true
}

// recordLive shows the turn so far on the panel.
func (s *Session) recordLive(gen int, t *liveTurn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.gen {
		return
	}
	s.st.LastHeard = strings.TrimSpace(t.heard)
	s.st.LastReply = strings.TrimSpace(t.reply)
	s.st.LastError = ""
}

func (s *Session) liveError(gen int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen == s.gen {
		s.st.LastError = err.Error()
	}
}
