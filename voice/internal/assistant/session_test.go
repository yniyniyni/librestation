package assistant

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	"voice/internal/agent"
	"voice/internal/led/proto"
	"voice/internal/tts"
)

// fakeClock fires After channels only when the test advances it.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []fakeTimer
}

type fakeTimer struct {
	at time.Time
	d  time.Duration
	ch chan time.Time
}

func newClock() *fakeClock { return &fakeClock{now: time.Date(2026, 10, 31, 21, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	c.timers = append(c.timers, fakeTimer{at: c.now.Add(d), d: d, ch: ch})
	return ch
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	keep := c.timers[:0]
	for _, t := range c.timers {
		if t.at.After(c.now) {
			keep = append(keep, t)
		} else {
			t.ch <- c.now
		}
	}
	c.timers = keep
}

// pending reports whether a timer of duration d is waiting.
func (c *fakeClock) pending(d time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.timers {
		if t.d == d {
			return true
		}
	}
	return false
}

// llm answers each Stream with the next step.
type llm struct {
	mu    sync.Mutex
	steps []func(ctx context.Context, r agent.Request, onText func(string)) (agent.Content, error)
	reqs  []agent.Request
}

func (l *llm) Stream(ctx context.Context, r agent.Request, onText func(string)) (agent.Content, error) {
	l.mu.Lock()
	l.reqs = append(l.reqs, r)
	i := len(l.reqs) - 1
	l.mu.Unlock()
	if i >= len(l.steps) {
		return agent.Content{}, errors.New("llm: script exhausted")
	}
	return l.steps[i](ctx, r, onText)
}

func (l *llm) requests() []agent.Request {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.reqs)
}

func reply(text string) func(context.Context, agent.Request, func(string)) (agent.Content, error) {
	return func(_ context.Context, _ agent.Request, onText func(string)) (agent.Content, error) {
		onText(text)
		return agent.Content{Role: "model", Parts: []agent.Part{{Text: text}}}, nil
	}
}

func call(name string) func(context.Context, agent.Request, func(string)) (agent.Content, error) {
	return func(context.Context, agent.Request, func(string)) (agent.Content, error) {
		return agent.Content{Role: "model", Parts: []agent.Part{{Call: &agent.Call{Name: name}}}}, nil
	}
}

type fakeTTS struct {
	mu    sync.Mutex
	texts []string
}

func (f *fakeTTS) Synth(_ context.Context, text string) ([]int16, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.texts = append(f.texts, text)
	pcm := make([]int16, 2400)
	pcm[0] = int16(len(f.texts)) // which synthesis this was
	return pcm, 24000, nil
}

func (f *fakeTTS) said() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.texts)
}

// player records plays; while hold is set each Play waits for release or
// its ctx.
type player struct {
	mu        sync.Mutex
	plays     int
	order     []int16 // pcm[0] of each play
	cancelled int
	hold      chan struct{}
}

func (p *player) Play(ctx context.Context, pcm []int16, rate int) error {
	p.mu.Lock()
	p.plays++
	p.order = append(p.order, pcm[0])
	hold := p.hold
	p.mu.Unlock()
	if hold == nil {
		return nil
	}
	select {
	case <-hold:
		return nil
	case <-ctx.Done():
		p.mu.Lock()
		p.cancelled++
		p.mu.Unlock()
		return ctx.Err()
	}
}

func (p *player) counts() (plays, cancelled int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.plays, p.cancelled
}

// rig is a session with fakes all round.
type rig struct {
	t      *testing.T
	s      *Session
	clock  *fakeClock
	llm    *llm
	tts    *fakeTTS
	player *player
	r      *rand.Rand

	mu     sync.Mutex
	leds   []proto.Event
	ducks  []float64
	events []string // LED states and ducks, in order
	stocks []string
}

func newRig(t *testing.T, steps ...func(context.Context, agent.Request, func(string)) (agent.Content, error)) *rig {
	g := &rig{t: t, clock: newClock(), llm: &llm{steps: steps}, tts: &fakeTTS{}, player: &player{}, r: rand.New(rand.NewSource(3))}
	g.s = New(Deps{
		Agent:  &agent.Agent{LLM: g.llm, Now: g.clock.Now},
		TTS:    func() tts.Engine { return g.tts },
		Player: g.player,
		Duck: func(f float64) {
			g.mu.Lock()
			g.ducks = append(g.ducks, f)
			g.events = append(g.events, fmt.Sprintf("duck %g", f))
			g.mu.Unlock()
		},
		LED: func(e proto.Event) {
			g.mu.Lock()
			g.leds = append(g.leds, e)
			g.events = append(g.events, "led "+map[bool]string{true: e.A, false: "off"}[e.On])
			g.mu.Unlock()
		},
		Stock: func(name string) ([]int16, int, error) {
			g.mu.Lock()
			g.stocks = append(g.stocks, name)
			g.mu.Unlock()
			return make([]int16, 1600), 16000, nil
		},
		Now: g.clock.Now,
	})
	g.s.after = g.clock.After
	return g
}

// wake starts a request; the test's end stops the session and waits for it.
func (g *rig) wake(onNoRequest func()) {
	ctx, cancel := context.WithCancel(context.Background())
	g.s.Wake(ctx, onNoRequest)
	g.t.Cleanup(func() {
		cancel()
		g.s.wait()
	})
}

func (g *rig) until(what string, cond func() bool) {
	g.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			g.t.Fatalf("timed out waiting for %s (state %s)", what, g.s.Status().State)
		}
		time.Sleep(time.Millisecond)
	}
}

func (g *rig) untilListening() {
	g.t.Helper()
	g.until("listening", g.s.listening)
}

func (g *rig) untilState(st string) {
	g.t.Helper()
	g.until(st, func() bool { return g.s.Status().State == st })
}

// say feeds pcm the way the audio goroutine does, in odd-sized blocks.
func (g *rig) say(pcm []int16) {
	for i := 0; i < len(pcm); i += block {
		g.s.Audio(pcm[i:min(i+block, len(pcm))])
	}
}

// utterance is a request: a little room noise, 1 s of speech, then silence.
func (g *rig) utterance() []int16 {
	return concat(noise(g.r, 500*time.Millisecond, quietDB), tone(time.Second, speechDB), noise(g.r, time.Second, quietDB))
}

func (g *rig) ledStates() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, e := range g.leds {
		if e.T != "ai" {
			continue
		}
		s := "off"
		if e.On {
			s = e.A
		}
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}

func (g *rig) duckLog() []float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.ducks)
}

func (g *rig) stockLog() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.stocks)
}

func audioLen(r agent.Request) time.Duration {
	last := r.Contents[len(r.Contents)-1]
	return time.Duration(len(last.Parts[0].Audio)) * time.Second / rate
}

func TestHappyPath(t *testing.T) {
	g := newRig(t, reply("Ставлю."))
	g.wake(func() { t.Error("onNoRequest called") })
	if !g.s.Busy() {
		t.Error("not busy right after Wake")
	}
	g.untilListening()
	g.say(g.utterance())
	g.until("reply played", func() bool { p, _ := g.player.counts(); return p == 1 })
	g.until("follow-up wait", func() bool { return g.clock.pending(followDelay) })
	g.clock.Advance(followDelay)
	g.untilState("followup")
	g.untilListening()
	g.say(noise(g.r, followLimit+100*time.Millisecond, quietDB))
	g.untilState("idle")

	if got := g.tts.said(); !slices.Equal(got, []string{"Ставлю."}) {
		t.Errorf("tts got %q", got)
	}
	if got, want := g.ledStates(), []string{"listening", "thinking", "speaking", "listening", "off"}; !slices.Equal(got, want) {
		t.Errorf("led %v, want %v", got, want)
	}
	if d := g.duckLog(); len(d) < 2 || d[0] != duckGain || d[len(d)-1] != 1 || slices.Contains(d[:len(d)-1], 1) {
		t.Errorf("duck %v, want 0.2 … 1", d)
	}
	if reqs := g.llm.requests(); len(reqs) != 1 || !near(audioLen(reqs[0]), 1450*time.Millisecond, 60*time.Millisecond) {
		t.Errorf("requests %d, audio %v", len(reqs), audioLen(reqs[0]))
	}
	st := g.s.Status()
	if st.LastReply != "Ставлю." || st.LastError != "" || g.s.Busy() {
		t.Errorf("status %+v, busy %v", st, g.s.Busy())
	}
}

func TestNoRequestIsSilentAndMarksFalse(t *testing.T) {
	g := newRig(t, call("no_request"))
	var marked sync.WaitGroup
	marked.Add(1)
	g.wake(marked.Done)
	g.untilListening()
	g.say(g.utterance())
	marked.Wait()
	g.untilState("idle")
	if p, _ := g.player.counts(); p != 0 || len(g.tts.said()) != 0 {
		t.Errorf("plays %d, tts %q", p, g.tts.said())
	}
	if d := g.duckLog(); d[len(d)-1] != 1 {
		t.Errorf("duck %v, want restored", d)
	}
}

func TestNoSpeechMarksFalse(t *testing.T) {
	g := newRig(t)
	var marked sync.WaitGroup
	marked.Add(1)
	g.wake(marked.Done)
	g.untilListening()
	g.say(noise(g.r, startLimit+100*time.Millisecond, quietDB))
	marked.Wait()
	g.untilState("idle")
	if n := len(g.llm.requests()); n != 0 {
		t.Errorf("%d model calls without speech", n)
	}
}

func TestModelErrorPlaysStockAndIdles(t *testing.T) {
	g := newRig(t, func(context.Context, agent.Request, func(string)) (agent.Content, error) {
		return agent.Content{}, &url.Error{Op: "Post", URL: "https://example.invalid", Err: errors.New("dial tcp: connection refused")}
	})
	g.wake(nil)
	g.untilListening()
	g.say(g.utterance())
	g.until("stock", func() bool { return len(g.stockLog()) == 1 })
	g.until("idle", func() bool { return g.s.Status().State == "idle" && g.clock.Now().Sub(g.s.Status().At) < time.Second })
	if got := g.stockLog(); got[0] != "no_internet" {
		t.Errorf("stock %q", got)
	}
	if p, _ := g.player.counts(); p != 1 {
		t.Errorf("plays %d, want the stock phrase", p)
	}
	if st := g.s.Status(); st.LastError == "" {
		t.Error("no LastError")
	}
	if !slices.Contains(g.ledStates(), "error") {
		t.Errorf("led %v, want error", g.ledStates())
	}
}

func TestOtherErrorPlaysSomethingWentWrong(t *testing.T) {
	g := newRig(t, func(context.Context, agent.Request, func(string)) (agent.Content, error) {
		return agent.Content{}, errors.New("gemini: 400 bad request")
	})
	g.wake(nil)
	g.untilListening()
	g.say(g.utterance())
	g.untilState("idle")
	if got := g.stockLog(); !slices.Equal(got, []string{"something_went_wrong"}) {
		t.Errorf("stock %q", got)
	}
}

func TestFollowUpStartsAfterPlaybackDrains(t *testing.T) {
	g := newRig(t, reply("Ставлю."), reply("Готово."))
	g.player.hold = make(chan struct{})
	g.wake(nil)
	g.untilListening()
	g.say(g.utterance())
	g.untilState("speaking")
	g.say(g.utterance()) // while speaking: ignored
	close(g.player.hold)
	g.until("follow-up wait", func() bool { return g.clock.pending(followDelay) })
	g.say(g.utterance()) // during the pause: ignored
	if g.s.listening() {
		t.Fatal("listening during the pause after playback")
	}
	g.clock.Advance(followDelay)
	g.untilState("followup")
	g.untilListening()
	g.say(g.utterance())
	g.until("second reply", func() bool { return len(g.tts.said()) == 2 })

	reqs := g.llm.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d model calls, want 2", len(reqs))
	}
	if n := len(reqs[1].Contents); n != 3 {
		t.Errorf("second turn has %d contents, want the first turn's history and new audio", n)
	}
	if d := audioLen(reqs[1]); !near(d, 1450*time.Millisecond, 60*time.Millisecond) {
		t.Errorf("follow-up audio %v: audio from before the follow-up leaked in", d)
	}
}

func TestTriggerDuringSpeechBargesIn(t *testing.T) {
	g := newRig(t, reply("Ставлю."), reply("Хорошо."))
	g.player.hold = make(chan struct{})
	g.wake(nil)
	g.untilListening()
	g.say(g.utterance())
	g.untilState("speaking")
	g.wake(nil)
	g.until("playback cancelled", func() bool { _, c := g.player.counts(); return c == 1 })
	g.untilListening()
	if st := g.s.Status().State; st != "listening" {
		t.Errorf("state %s after barge-in, want listening", st)
	}
	close(g.player.hold)
	g.say(g.utterance())
	g.until("second reply", func() bool { return len(g.tts.said()) == 2 })
	if reqs := g.llm.requests(); len(reqs) != 2 || len(reqs[1].Contents) != 3 {
		t.Errorf("second turn should carry the first turn's history")
	}
}

func TestAudioNeverBlocks(t *testing.T) {
	stuck := make(chan struct{})
	g := newRig(t, func(ctx context.Context, _ agent.Request, _ func(string)) (agent.Content, error) {
		close(stuck)
		<-ctx.Done()
		return agent.Content{}, ctx.Err()
	})
	g.wake(nil)
	g.untilListening()
	g.say(g.utterance())
	<-stuck
	blk := noise(g.r, 32*time.Millisecond, quietDB)
	for i := 0; i < 500; i++ {
		start := time.Now()
		g.s.Audio(blk)
		if d := time.Since(start); d > time.Millisecond {
			t.Fatalf("Audio took %v while Turn is stuck", d)
		}
	}
}

func TestStartConversationNotReadyYet(t *testing.T) {
	g := newRig(t, call("start_conversation"))
	g.wake(func() { t.Error("onNoRequest called") })
	g.untilListening()
	g.say(g.utterance())
	g.until("follow-up wait", func() bool { return g.clock.pending(followDelay) })
	if got := g.tts.said(); !slices.Equal(got, []string{notReady}) {
		t.Errorf("tts got %q", got)
	}
	g.clock.Advance(followDelay)
	g.untilState("followup")
}

func (g *rig) eventLog() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.events)
}

// stuck is a model call that hangs until cancelled; started is closed when
// it begins.
func stuck(started chan struct{}) func(context.Context, agent.Request, func(string)) (agent.Content, error) {
	return func(ctx context.Context, _ agent.Request, _ func(string)) (agent.Content, error) {
		close(started)
		<-ctx.Done()
		return agent.Content{}, ctx.Err()
	}
}

// toFollowUp runs one request through its reply into the follow-up listen.
func (g *rig) toFollowUp() {
	g.t.Helper()
	g.untilListening()
	g.say(g.utterance())
	g.until("follow-up wait", func() bool { return g.clock.pending(followDelay) })
	g.clock.Advance(followDelay)
	g.untilState("followup")
	g.untilListening()
}

func TestBargeInNoStaleLEDOrDuck(t *testing.T) {
	g := newRig(t, reply("Ставлю."), reply("Хорошо."))
	g.player.hold = make(chan struct{})
	g.wake(nil)
	g.untilListening()
	g.say(g.utterance())
	g.untilState("speaking")
	g.wake(nil)
	g.until("playback cancelled", func() bool { _, c := g.player.counts(); return c == 1 })
	g.untilListening()
	g.until("new run's LED", func() bool { ev := g.eventLog(); return ev[len(ev)-1] == "led listening" })
	ev := g.eventLog()
	if slices.Contains(ev, "led off") || slices.Contains(ev, "duck 1") {
		t.Errorf("events %v: the cancelled run went idle", ev)
	}
	if want := []string{"duck 0.2", "led listening"}; !slices.Equal(ev[len(ev)-2:], want) {
		t.Errorf("events %v, want the new run to end with %v", ev, want)
	}
}

func TestWakeWhileThinkingDropsInFlightTurn(t *testing.T) {
	started := make(chan struct{})
	g := newRig(t, reply("Ставлю."), stuck(started), reply("Хорошо."))
	g.wake(nil)
	g.toFollowUp()
	g.say(g.utterance())
	<-started
	g.wake(nil)
	g.untilListening()
	g.say(g.utterance())
	g.until("third reply", func() bool { return len(g.tts.said()) == 2 })
	reqs := g.llm.requests()
	if len(reqs) != 3 {
		t.Fatalf("%d model calls, want 3", len(reqs))
	}
	// The first turn (audio + reply) is kept; the cancelled one is not.
	if n := len(reqs[2].Contents); n != 3 {
		t.Errorf("third turn has %d contents, want 3", n)
	}
}

func TestWakeFromIdleFreshHistory(t *testing.T) {
	g := newRig(t, reply("Ставлю."), reply("Хорошо."))
	g.wake(nil)
	g.toFollowUp()
	g.say(noise(g.r, followLimit+100*time.Millisecond, quietDB))
	g.untilState("idle")
	g.wake(nil)
	g.untilListening()
	g.say(g.utterance())
	g.until("second reply", func() bool { return len(g.tts.said()) == 2 })
	if reqs := g.llm.requests(); len(reqs) != 2 || len(reqs[1].Contents) != 1 {
		t.Errorf("a wake from idle should start without history")
	}
}

func TestFollowUpNoSpeechClearsHistory(t *testing.T) {
	g := newRig(t, reply("Ставлю."))
	g.wake(nil)
	g.toFollowUp()
	g.s.mu.Lock()
	had := len(g.s.hist)
	g.s.mu.Unlock()
	g.say(noise(g.r, followLimit+100*time.Millisecond, quietDB))
	g.untilState("idle")
	g.s.mu.Lock()
	left := len(g.s.hist)
	g.s.mu.Unlock()
	if had == 0 || left != 0 {
		t.Errorf("history %d during follow-up, %d after; want kept, then cleared", had, left)
	}
	if d := g.duckLog(); d[len(d)-1] != 1 {
		t.Errorf("duck %v, want restored", d)
	}
}

func TestAudioStallGoesIdleQuietly(t *testing.T) {
	g := newRig(t)
	g.wake(func() { t.Error("onNoRequest called for a stall") })
	g.untilListening()
	stall := startLimit + maxUtterance + listenSlack
	g.until("guard", func() bool { return g.clock.pending(stall) })
	g.clock.Advance(stall)
	g.untilState("idle")
	if p, _ := g.player.counts(); p != 0 || len(g.stockLog()) != 0 {
		t.Errorf("plays %d, stock %q; want silence", p, g.stockLog())
	}
}

func TestMultiSentenceReplyPipelined(t *testing.T) {
	g := newRig(t, reply("Ставлю. Готово."))
	g.player.hold = make(chan struct{})
	g.wake(nil)
	g.untilListening()
	g.say(g.utterance())
	g.untilState("speaking")
	// Sentence 2 is synthesised while sentence 1 is still playing.
	g.until("second synthesis", func() bool { return len(g.tts.said()) == 2 })
	if p, _ := g.player.counts(); p != 1 {
		t.Errorf("plays %d while the first is held, want 1", p)
	}
	close(g.player.hold)
	g.until("both played", func() bool { p, _ := g.player.counts(); return p == 2 })
	if got := g.tts.said(); !slices.Equal(got, []string{"Ставлю.", "Готово."}) {
		t.Errorf("tts got %q", got)
	}
	g.player.mu.Lock()
	order := slices.Clone(g.player.order)
	g.player.mu.Unlock()
	if !slices.Equal(order, []int16{1, 2}) {
		t.Errorf("play order %v, want [1 2]", order)
	}
}
