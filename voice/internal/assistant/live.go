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
	// liveDialLimit bounds the first dial of a conversation: past it the
	// agent takes over, with what the user said meanwhile.
	liveDialLimit = 3 * time.Second
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

// liveOn makes LiveAudio queue for sending, starting with the pre-roll when
// seed is set, and arms the voice watch. It returns the pre-roll's length.
func (s *Session) liveOn(seed bool) int {
	s.amu.Lock()
	s.live, s.lq = true, s.lq[:0]
	if seed {
		s.lq = s.lraw.appendTo(s.lq)
	}
	n := len(s.lq)
	s.watch.arm()
	s.amu.Unlock()
	poke(s.lnote)
	return n
}

// liveLeft is what a failed first dial leaves for the agent: the pre-roll
// and what the user said while it was dialling, both from channel 0.
type liveLeft struct {
	tried       bool // the conversation tried Live first
	seed, heard []int16
}

// liveTake stops the queue and hands back what was in it; seeded samples at
// its front are the pre-roll. The queue holds more than a dial may take, so
// nothing was dropped from it.
func (s *Session) liveTake(seeded int) liveLeft {
	s.amu.Lock()
	defer s.amu.Unlock()
	q := append([]int16(nil), s.lq...)
	s.live, s.lq = false, s.lq[:0]
	seeded = min(seeded, len(q))
	return liveLeft{tried: true, seed: q[:seeded], heard: q[seeded:]}
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
	userAt    time.Time // when the user was last heard to start or transcribed
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
	conn *liveConn // the connection the calls came on
	ids  []string
	err  error // from Respond
}

// runLive holds the conversation over one Live session. It reports false,
// having done nothing else, when Live does not connect within liveDialLimit:
// the conversation then goes to the agent, with what Live would have heard.
func (s *Session) runLive(ctx context.Context, gen int, lp livePlayer, seed bool, onNoRequest func()) (bool, liveLeft) {
	seeded := s.liveOn(seed) // audio queues while the connection opens
	started := time.Now()
	c, err := s.dialFirst(ctx)
	if err != nil {
		left := s.liveTake(seeded)
		if ctx.Err() != nil {
			s.idle(gen)
			return true, liveLeft{}
		}
		log.Printf("assistant: Live unavailable, using the agent: %v", err)
		return false, left
	}
	log.Printf("assistant: Live session open in %v", time.Since(started).Round(time.Millisecond))
	r := &liveRun{s: s, gen: gen, ctx: ctx, lp: lp, onNoRequest: onNoRequest,
		byName: map[string]agent.Tool{}, cancels: map[string]context.CancelFunc{}, toolDone: make(chan toolBatch)}
	for _, t := range s.d.Agent.Tools {
		r.byName[t.Decl().Name] = t
	}
	r.run(c)
	return true, liveLeft{}
}

// dialFirst opens the conversation's Live session, giving up after
// liveDialLimit. The bound only cancels a dial still under way: the context
// of one that succeeded is left alone, as the connection may depend on it.
func (s *Session) dialFirst(ctx context.Context) (LiveSession, error) {
	dctx, cancel := context.WithCancel(ctx)
	var mu sync.Mutex
	dialled, late := false, false
	stop := make(chan struct{})
	go func() {
		select {
		case <-s.after(liveDialLimit):
			mu.Lock()
			if !dialled {
				late = true
				cancel()
			}
			mu.Unlock()
		case <-stop:
		}
	}()
	c, err := s.d.Live(dctx, "")
	mu.Lock()
	dialled = true
	mu.Unlock()
	close(stop)
	if late {
		if err == nil {
			c.Close()
		}
		return nil, fmt.Errorf("assistant: Live did not connect in %v", liveDialLimit)
	}
	if err != nil {
		cancel()
	}
	return c, err
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
			if r.user && r.s.d.Now().Sub(r.userAt) < maxUtterance {
				r.listen(r.wait) // not while the user speaks
				continue
			}
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
	r.user = false // the reply's echo is forgotten with the watch's past
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
			r.userAt = r.s.d.Now()
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
	if ev&evStart != 0 {
		r.userAt = r.s.d.Now()
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
	if r.state == "speaking" || r.finishing {
		r.out.flush() // the reply is not coming back
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
	lc := r.conn
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
			err = lc.c.Respond(r.tctx, resps)
		}
		select {
		case r.toolDone <- toolBatch{lc, ids, err}:
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
		if b.conn != r.conn {
			// The calls came on a connection since replaced: its answer is
			// lost with it, the conversation goes on.
			log.Printf("assistant: Live: tool answer for a replaced connection: %v", b.err)
		} else {
			return r.lost(b.err)
		}
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
