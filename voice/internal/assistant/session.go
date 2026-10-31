// Package assistant runs one spoken request at a time: it listens for the
// utterance after the wake word, hands it to the agent, speaks the reply and
// keeps listening for a follow-up.
package assistant

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"voice/internal/agent"
	"voice/internal/led/proto"
	"voice/internal/tts"
)

const (
	// duckGain is the music's gain while the assistant is active; it matches
	// duckGain in cmd/voiced.
	duckGain = 0.2

	// startLimit is how long to wait for speech after the wake word.
	startLimit = 4 * time.Second
	// followLimit is how long to wait for a follow-up after a reply.
	followLimit = 5 * time.Second
	// followDelay is the pause after playback before the follow-up listens,
	// so the reply's tail does not open it.
	followDelay = 300 * time.Millisecond
	// listenSlack is added to the audio-time limits for a wall-clock guard,
	// should audio stop arriving.
	listenSlack = 2 * time.Second

	turnTimeout  = 20 * time.Second
	synthTimeout = 15 * time.Second
	// speakLimit bounds one reply, from the model call to the end of
	// playback: a player that never returns must not keep the session busy.
	speakLimit = 60 * time.Second

	// maxBuffered bounds the audio waiting for the session's goroutine.
	maxBuffered = 12 * sampleRate
	// maxQueued bounds the sentences waiting for synthesis.
	maxQueued = 64
	// keepTurns is how many past turns the conversation keeps; each carries
	// its audio.
	keepTurns = 3

	notReady = "Режим разговора пока не готов"
)

var errSpeakStuck = errors.New("assistant: reply playback stuck")

// Player plays PCM at its rate; it returns when the audio has had time to
// play, or early when ctx is cancelled.
type Player interface {
	Play(ctx context.Context, pcm []int16, rate int) error
}

// Deps are the session's collaborators. Duck, LED and Stock may be nil.
type Deps struct {
	Agent  *agent.Agent
	TTS    func() tts.Engine // the engine chosen in the panel
	Player Player
	Duck   func(float64)     // music gain factor; 1 = not ducked
	LED    func(proto.Event) // ring state
	Stock  func(name string) ([]int16, int, error)
	Live   func(context.Context) (LiveSession, error)
	Now    func() time.Time // nil = time.Now
}

// Status is the session's state for the panel.
type Status struct {
	State     string    `json:"state"`      // idle|listening|thinking|speaking|followup
	LastHeard string    `json:"last_heard"` // the tools run; there is no transcript
	LastReply string    `json:"last_reply"`
	LastError string    `json:"last_error"`
	At        time.Time `json:"at"`
}

// Session is the assistant's state machine. Each Wake starts a run on its
// own goroutine; a new Wake cancels the previous run and starts after it has
// stopped, so runs never overlap.
type Session struct {
	d     Deps
	after func(time.Duration) <-chan time.Time

	emu    sync.Mutex // orders duck/LED events: a gen check and its callback are one step
	mu     sync.Mutex
	gen    int // the current run; stale runs change nothing
	st     Status
	hist   []agent.Content // the conversation so far, from completed turns
	cancel context.CancelFunc
	done   chan struct{}

	amu    sync.Mutex // guards the audio fields; held only for appends and swaps
	open   bool       // audio is wanted
	lv     levels     // fed always, for the noise floor
	raw    recent     // fed always, to seed the listen after a wake
	seed   []int16    // raw, copied out when a listen opens
	buf    []int16
	spare  []int16
	notify chan struct{}
}

func New(d Deps) *Session {
	if d.Now == nil {
		d.Now = time.Now
	}
	s := &Session{
		d:      d,
		after:  time.After,
		buf:    make([]int16, 0, maxBuffered),
		spare:  make([]int16, 0, maxBuffered),
		seed:   make([]int16, 0, seedLen),
		notify: make(chan struct{}, 1),
	}
	s.st = Status{State: "idle", At: d.Now()}
	return s
}

// Audio takes a 16 kHz block from the audio goroutine. It never blocks:
// the block's level is noted, and the block is copied only while the session
// listens, and dropped otherwise or when the buffer is full.
func (s *Session) Audio(pcm []int16) {
	s.amu.Lock()
	s.lv.push(pcm)
	s.raw.push(pcm)
	if s.open {
		n := min(len(pcm), maxBuffered-len(s.buf))
		s.buf = append(s.buf, pcm[:n]...)
	}
	open := s.open
	s.amu.Unlock()
	if open {
		select {
		case s.notify <- struct{}{}:
		default:
		}
	}
}

// Wake starts a request. While a request is running it cancels it (playback
// included, for barge-in) and listens again, keeping the conversation; from
// idle it starts fresh. onNoRequest is called when the request turns out to
// hold nothing for the speaker.
func (s *Session) Wake(ctx context.Context, onNoRequest func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	s.mu.Lock()
	s.gen++
	gen := s.gen
	prevCancel, prevDone := s.cancel, s.done
	hist := s.hist
	// A barge-in's seed would be the reply's echo: only a wake from idle
	// gets one.
	fromIdle := s.st.State == "idle"
	src := "barge-in"
	if fromIdle {
		src = "idle"
	}
	log.Printf("assistant: wake %s", src)
	if fromIdle {
		hist = nil
	}
	s.cancel, s.done = cancel, done
	s.setLocked("listening")
	s.mu.Unlock()
	if prevCancel != nil {
		prevCancel()
	}
	go func() {
		defer close(done)
		defer cancel()
		if prevDone != nil {
			<-prevDone
		}
		s.run(ctx, gen, hist, fromIdle, onNoRequest)
	}()
}

// Hush stops a running request: the reply is cut off, the music comes back,
// the ring goes off and the conversation is forgotten. It does not wait for
// the cancelled run, which changes nothing from here on (its events and its
// onNoRequest are dropped); a later Wake starts
// after it has stopped. From idle it does nothing.
func (s *Session) Hush() {
	s.mu.Lock()
	if s.st.State == "idle" {
		s.mu.Unlock()
		return
	}
	s.gen++ // the cancelled run is stale from now on
	if s.cancel != nil {
		s.cancel()
	}
	s.setLocked("idle")
	s.hist = nil
	s.mu.Unlock()
	log.Printf("assistant: hushed")
	// A stale run may be inside a duck or LED callback; emu makes ours
	// land after it, and later stale calls see the new gen.
	s.emu.Lock()
	defer s.emu.Unlock()
	if s.d.Duck != nil {
		s.d.Duck(1)
	}
	if s.d.LED != nil {
		s.d.LED(proto.Event{T: "ai"})
	}
}

// State is Status().State: idle|listening|thinking|speaking|followup.
func (s *Session) State() string { return s.Status().State }

// Busy is true from Wake until the session is idle again.
func (s *Session) Busy() bool { return s.State() != "idle" }

func (s *Session) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st
}

// wait blocks until the current run has stopped.
func (s *Session) wait() {
	s.mu.Lock()
	done := s.done
	s.mu.Unlock()
	if done != nil {
		<-done
	}
}

// listening reports whether Audio is being taken.
func (s *Session) listening() bool {
	s.amu.Lock()
	defer s.amu.Unlock()
	return s.open
}

func (s *Session) run(ctx context.Context, gen int, hist []agent.Content, seed bool, onNoRequest func()) {
	s.duck(gen, duckGain)
	if s.d.Live != nil {
		s.runLive(ctx, gen, onNoRequest)
		return
	}
	noRequest := func(first bool) {
		if first && onNoRequest != nil && s.current(gen) {
			onNoRequest()
		}
	}
	for first := true; ; first = false {
		limit, state := startLimit, "listening"
		if !first {
			limit, state = followLimit, "followup"
		}
		// The first listen after a wake from idle takes the audio from just
		// before it opened as pre-roll; a follow-up's would be the reply's
		// tail.
		audio, res := s.listen(ctx, gen, state, limit, first && seed)
		if ctx.Err() != nil || res == epStalled {
			s.idle(gen)
			return
		}
		if res == epNoSpeech {
			noRequest(first)
			s.idle(gen)
			return
		}
		speechEnd := time.Now()
		log.Printf("assistant: heard %d ms", len(audio)*1000/16000)
		s.set(gen, "thinking", "thinking")

		sp := s.speak(ctx, gen, speechEnd)
		tctx, cancel := context.WithTimeout(ctx, turnTimeout)
		h, out, err := s.d.Agent.Turn(tctx, hist, audio, sp.say)
		cancel()
		turnDur := time.Since(speechEnd).Round(time.Millisecond)
		if err != nil {
			sp.stop()
		} else {
			// The turn is complete: a barge-in during the reply keeps it.
			h = trimHistory(h)
			hist = h
			s.mu.Lock()
			if gen == s.gen {
				s.hist = h
			}
			s.mu.Unlock()
			if out.StartConversation {
				sp.say(notReady) // plan 3 replaces this
			}
		}
		terr := sp.finish()
		logTurn(out, err, terr, turnDur)
		if ctx.Err() != nil {
			s.idle(gen)
			return
		}
		if errors.Is(terr, errSpeakStuck) {
			// No stock phrase: it would go to the same stuck player.
			log.Printf("%v", terr)
			s.record(gen, out, terr)
			s.idle(gen)
			return
		}
		if err == nil {
			err = terr
		}
		s.record(gen, out, err)
		if err != nil {
			log.Printf("assistant: %v", err)
			s.fail(ctx, gen, err)
			return
		}
		if out.NoRequest {
			noRequest(first)
			s.idle(gen)
			return
		}
		select {
		case <-ctx.Done():
			s.idle(gen)
			return
		case <-s.after(followDelay):
		}
	}
}

// logTurn logs how a turn came out: its duration up to the model's last
// word, then the error, no_request or the number of characters spoken.
func logTurn(out agent.Outcome, err, terr error, d time.Duration) {
	res := fmt.Sprintf("spoken %d chars", utf8.RuneCountInString(out.Spoken))
	switch {
	case err != nil:
		res = "error: " + err.Error()
	case terr != nil:
		res = fmt.Sprintf("%s, speech error: %v", res, terr)
	case out.NoRequest:
		res = "no_request"
	}
	log.Printf("assistant: turn %v: %s", d, res)
}

// listen takes audio until the endpointer has an utterance (epDone), finds
// none (epNoSpeech) or audio stops (epStalled). A cancel gives epPending.
// seed gives it the last seedLen samples from before it opened, as pre-roll.
func (s *Session) listen(ctx context.Context, gen int, state string, limit time.Duration, seed bool) ([]int16, epResult) {
	s.amu.Lock()
	s.buf, s.open = s.buf[:0], true
	s.seed = s.seed[:0]
	if seed {
		s.seed = s.raw.appendTo(s.seed)
	}
	floor := s.lv.floor()
	s.amu.Unlock()
	defer func() {
		s.amu.Lock()
		s.buf, s.open = s.buf[:0], false
		s.amu.Unlock()
	}()
	s.set(gen, state, "listening")
	ep := newEndpointer(limit, floor, s.seed)
	guard := s.after(limit + maxUtterance + listenSlack)
	for {
		select {
		case <-ctx.Done():
			return nil, epPending
		case <-guard:
			log.Printf("assistant: audio stalled while listening")
			return nil, epStalled
		case <-s.notify:
		}
		s.amu.Lock()
		blk := s.buf
		s.buf, s.spare = s.spare[:0], blk
		s.amu.Unlock()
		switch r := ep.push(blk); r {
		case epNoSpeech:
			return nil, r
		case epDone:
			return ep.utterance(), r
		}
	}
}

// trimHistory keeps the last keepTurns turns. It cuts only where a turn
// starts, at the user's audio, so no call is left without its result.
func trimHistory(h []agent.Content) []agent.Content {
	turns := 0
	for i := len(h) - 1; i >= 0; i-- {
		if !isUserAudio(h[i]) {
			continue
		}
		if turns++; turns == keepTurns {
			return slices.Clone(h[i:]) // let go of the older audio
		}
	}
	return h
}

func isUserAudio(c agent.Content) bool {
	return c.Role == "user" && len(c.Parts) > 0 && c.Parts[0].Audio != nil
}

// fail tells the user something went wrong and goes idle.
func (s *Session) fail(ctx context.Context, gen int, err error) {
	s.led(gen, proto.Event{T: "ai", On: true, A: "error"})
	name := "something_went_wrong"
	if isNetwork(err) {
		name = "no_internet"
	}
	log.Printf("assistant: fail path, saying %s", name)
	pcm, rate, err := s.stock(ctx, name)
	if err != nil {
		log.Printf("assistant: %s: %v", name, err)
	} else if err := s.d.Player.Play(ctx, pcm, rate); err != nil && ctx.Err() == nil {
		log.Printf("assistant: play %s: %v", name, err)
	}
	s.idle(gen)
}

// stockLines are said through the TTS when a stock phrase will not load;
// Piper is local, so they work offline.
var stockLines = map[string]string{
	"no_internet":          "Нет связи с интернетом.",
	"something_went_wrong": "Что-то пошло не так.",
}

// stock loads the stock phrase name, or synthesises its line instead.
func (s *Session) stock(ctx context.Context, name string) ([]int16, int, error) {
	if s.d.Stock != nil {
		pcm, rate, err := s.d.Stock(name)
		if err == nil {
			return pcm, rate, nil
		}
		log.Printf("assistant: stock %s: %v", name, err)
	}
	return s.synth(ctx, stockLines[name])
}

func isNetwork(err error) bool {
	var ue *url.Error
	var oe *net.OpError
	return errors.As(err, &ue) || errors.As(err, &oe) || errors.Is(err, context.DeadlineExceeded)
}

// record keeps the turn's result for the panel.
func (s *Session) record(gen int, out agent.Outcome, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.gen {
		return
	}
	s.st.LastHeard = strings.Join(out.Calls, ", ")
	s.st.LastReply = out.Spoken
	s.st.LastError = ""
	if err != nil {
		s.st.LastError = err.Error()
		s.hist = nil // it may end with a call the model would reject unanswered
	}
}

// set moves the current run to state and shows led on the ring.
func (s *Session) set(gen int, state, led string) {
	s.mu.Lock()
	if gen != s.gen {
		s.mu.Unlock()
		return
	}
	s.setLocked(state)
	s.mu.Unlock()
	s.led(gen, proto.Event{T: "ai", On: true, A: led})
}

func (s *Session) setLocked(state string) {
	s.st.State, s.st.At = state, s.d.Now()
}

// idle ends the current run: the music comes back, the ring goes off and the
// conversation is forgotten.
func (s *Session) idle(gen int) {
	s.mu.Lock()
	if gen != s.gen {
		s.mu.Unlock()
		return
	}
	s.setLocked("idle")
	s.hist = nil
	s.mu.Unlock()
	s.duck(gen, 1)
	s.led(gen, proto.Event{T: "ai"})
}

// current reports whether gen is still the current run.
func (s *Session) current(gen int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return gen == s.gen
}

// duck sets the music's gain unless gen is stale, as led does. The check and
// the call are one step under emu, so a stale run cannot land an event after
// Hush's.
func (s *Session) duck(gen int, f float64) {
	s.emu.Lock()
	defer s.emu.Unlock()
	if s.current(gen) && s.d.Duck != nil {
		s.d.Duck(f)
	}
}

// led sends e unless gen is stale; see duck. A new run starts after the old
// one has stopped, so a stale event cannot land after the new run's.
func (s *Session) led(gen int, e proto.Event) {
	s.emu.Lock()
	defer s.emu.Unlock()
	if s.current(gen) && s.d.LED != nil {
		s.d.LED(e)
	}
}

// speech synthesises queued sentences and plays them in order; sentence N+1
// is synthesised while N plays.
type speech struct {
	in      chan string
	cancel  context.CancelFunc
	done    chan struct{}
	expired chan struct{} // closed when speakLimit ran out before done

	mu  sync.Mutex
	err error
}

func (s *Session) speak(ctx context.Context, gen int, speechEnd time.Time) *speech {
	ctx, cancel := context.WithCancel(ctx)
	sp := &speech{in: make(chan string, maxQueued), cancel: cancel, done: make(chan struct{}), expired: make(chan struct{})}
	limit := s.after(speakLimit)
	go func() {
		select {
		case <-limit:
			cancel()
			close(sp.expired)
		case <-sp.done:
		}
	}()
	type clip struct {
		pcm  []int16
		rate int
	}
	clips := make(chan clip, 1)
	go func() {
		defer close(clips)
		for {
			var text string
			var ok bool
			select {
			case <-ctx.Done():
				return
			case text, ok = <-sp.in:
				if !ok {
					return
				}
			}
			pcm, rate, err := s.synth(ctx, text)
			if err != nil {
				if ctx.Err() == nil {
					sp.fail(err)
				}
				cancel()
				return
			}
			select {
			case clips <- clip{pcm, rate}:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		defer close(sp.done)
		first := true
		for c := range clips {
			if ctx.Err() != nil {
				continue
			}
			if first {
				s.set(gen, "speaking", "speaking")
				first = false
				log.Printf("assistant: first play %v after end of speech", time.Since(speechEnd).Round(time.Millisecond))
			}
			if err := s.d.Player.Play(ctx, c.pcm, c.rate); err != nil && ctx.Err() == nil {
				log.Printf("assistant: play: %v", err)
			}
		}
	}()
	return sp
}

func (s *Session) synth(ctx context.Context, text string) ([]int16, int, error) {
	e := s.d.TTS()
	if e == nil {
		return nil, 0, errors.New("assistant: no tts engine")
	}
	ctx, cancel := context.WithTimeout(ctx, synthTimeout)
	defer cancel()
	return e.Synth(ctx, text)
}

// say queues a sentence; called from inside Turn, so it never blocks.
func (sp *speech) say(text string) {
	select {
	case sp.in <- text:
	default:
		log.Printf("assistant: speech queue full, dropped %q", text)
	}
}

func (sp *speech) fail(err error) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.err == nil {
		sp.err = err
	}
}

// stop drops what has not been played yet.
func (sp *speech) stop() { sp.cancel() }

// finish waits for the queue to play out and returns the first synthesis
// error, or errSpeakStuck if it did not play out within speakLimit.
func (sp *speech) finish() error {
	close(sp.in)
	select {
	case <-sp.done:
	case <-sp.expired:
		select {
		case <-sp.done: // it did finish, just at the bound
		default:
			return errSpeakStuck
		}
	}
	sp.cancel()
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.err
}
