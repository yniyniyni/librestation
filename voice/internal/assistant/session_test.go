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
	set    map[time.Duration]int // timers set, by duration
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
	if c.set == nil {
		c.set = map[time.Duration]int{}
	}
	c.set[d]++
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
	wedge     chan struct{} // while set, Play ignores ctx until it is closed
}

func (p *player) Play(ctx context.Context, pcm []int16, rate int) error {
	p.mu.Lock()
	p.plays++
	p.order = append(p.order, pcm[0])
	hold, wedge := p.hold, p.wedge
	p.mu.Unlock()
	if wedge != nil {
		<-wedge
		return nil
	}
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

	duckGate func(float64) // called before a duck is recorded; may block
}

func newRig(t *testing.T, steps ...func(context.Context, agent.Request, func(string)) (agent.Content, error)) *rig {
	g := &rig{t: t, clock: newClock(), llm: &llm{steps: steps}, tts: &fakeTTS{}, player: &player{}, r: rand.New(rand.NewSource(3))}
	g.s = New(Deps{
		Agent:  &agent.Agent{LLM: g.llm, Now: g.clock.Now},
		TTS:    func() tts.Engine { return g.tts },
		Player: g.player,
		Duck: func(f float64) {
			if g.duckGate != nil {
				g.duckGate(f)
			}
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
	if got := g.tts.said(); !slices.Equal(got, []string{alreadyTalking}) {
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

func TestStockMissingSpeaksFallbackLine(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{&url.Error{Op: "Post", URL: "https://example.invalid", Err: errors.New("dial tcp: connection refused")}, "Нет связи с интернетом."},
		{errors.New("gemini: 400 bad request"), "Что-то пошло не так."},
	} {
		g := newRig(t, func(context.Context, agent.Request, func(string)) (agent.Content, error) {
			return agent.Content{}, c.err
		})
		g.s.d.Stock = func(string) ([]int16, int, error) { return nil, 0, errors.New("wav: not a RIFF/WAVE file") }
		g.wake(nil)
		g.untilListening()
		g.say(g.utterance())
		g.untilState("idle")
		if got := g.tts.said(); !slices.Equal(got, []string{c.want}) {
			t.Errorf("%v: tts got %q, want %q", c.err, got, c.want)
		}
		if p, _ := g.player.counts(); p != 1 {
			t.Errorf("%v: plays %d, want the fallback line", c.err, p)
		}
	}
}

func TestRequestStartedBeforeWakeIsKept(t *testing.T) {
	g := newRig(t, reply("Лайкнула."))
	// Room noise, then the request begins 400 ms before the wake fires.
	g.say(noise(g.r, 2*time.Second, quietDB))
	speech := tone(time.Second, speechDB)
	g.say(speech[:samples(400*time.Millisecond)])
	g.wake(nil)
	g.untilListening()
	g.say(speech[samples(400*time.Millisecond):])
	g.say(noise(g.r, time.Second, quietDB))
	g.until("reply", func() bool { return len(g.tts.said()) == 1 })
	// Speech runs across the wake, so the whole seed (480 ms, whole frames:
	// 80 ms noise and 400 ms speech) leads, then the rest of the speech and
	// the hangover.
	if reqs := g.llm.requests(); len(reqs) != 1 || !near(audioLen(reqs[0]), 1230*time.Millisecond, 60*time.Millisecond) {
		t.Errorf("audio %v, want the whole request", audioLen(reqs[0]))
	}
}

func TestFollowUpIsNotSeeded(t *testing.T) {
	g := newRig(t, reply("Ставлю."), reply("Готово."))
	g.wake(nil)
	g.untilListening()
	g.say(g.utterance())
	g.until("follow-up wait", func() bool { return g.clock.pending(followDelay) })
	g.say(tone(400*time.Millisecond, speechDB)) // the reply's tail, before the follow-up opens
	g.clock.Advance(followDelay)
	g.untilState("followup")
	g.untilListening()
	g.say(g.utterance())
	g.until("second reply", func() bool { return len(g.tts.said()) == 2 })
	if reqs := g.llm.requests(); len(reqs) != 2 || !near(audioLen(reqs[1]), 1450*time.Millisecond, 60*time.Millisecond) {
		t.Errorf("follow-up audio %v: audio from before it opened leaked in", audioLen(reqs[1]))
	}
}

func TestHistoryKeepsLastThreeTurns(t *testing.T) {
	audio := func(v int16) agent.Content {
		return agent.Content{Role: "user", Parts: []agent.Part{{Audio: []int16{v}}}}
	}
	text := agent.Content{Role: "model", Parts: []agent.Part{{Text: "Ок."}}}
	callC := agent.Content{Role: "model", Parts: []agent.Part{{Call: &agent.Call{Name: "like"}}}}
	result := agent.Content{Role: "user", Parts: []agent.Part{{Result: &agent.Result{Name: "like"}}}}
	var h []agent.Content
	for i := int16(1); i <= 5; i++ {
		h = append(h, audio(i), callC, result, text)
	}
	got := trimHistory(h)
	if len(got) != 12 || got[0].Parts[0].Audio[0] != 3 {
		t.Fatalf("kept %d contents starting %+v, want the last 3 turns from audio 3", len(got), got[0])
	}
	if short := h[:8]; len(trimHistory(short)) != 8 {
		t.Errorf("two turns were cut")
	}
}

func TestHistoryCappedAcrossFollowUps(t *testing.T) {
	g := newRig(t, reply("1."), reply("2."), reply("3."), reply("4."), reply("5."))
	g.wake(nil)
	for i := 0; i < 4; i++ {
		g.toFollowUp()
	}
	g.say(g.utterance())
	g.until("fifth reply", func() bool { return len(g.tts.said()) == 5 })
	reqs := g.llm.requests()
	// Three earlier turns (audio + reply each) and the new audio.
	if n := len(reqs[4].Contents); n != 7 {
		t.Errorf("fifth turn has %d contents, want 7", n)
	}
}

func TestWedgedPlaybackGoesIdleAfterTheBound(t *testing.T) {
	g := newRig(t, reply("Ставлю."))
	wedge := make(chan struct{})
	g.player.wedge = wedge
	t.Cleanup(func() { close(wedge) }) // after the session has stopped: frees the stuck Play
	g.wake(nil)
	g.untilListening()
	g.say(g.utterance())
	g.untilState("speaking")
	g.until("speak bound", func() bool { return g.clock.pending(speakLimit) })
	g.clock.Advance(speakLimit)
	g.untilState("idle")
	if g.s.Busy() {
		t.Error("busy after the speak bound")
	}
	if st := g.s.Status(); st.LastError == "" {
		t.Error("no LastError for the stuck playback")
	}
	if len(g.stockLog()) != 0 {
		t.Errorf("stock %q played into a stuck player", g.stockLog())
	}
}

func TestTriggerPauseThenRequest(t *testing.T) {
	g := newRig(t, reply("Ставлю."))
	g.say(noise(g.r, 2*time.Second, quietDB))
	g.say(tone(500*time.Millisecond, speechDB)) // the trigger word
	g.say(noise(g.r, 200*time.Millisecond, quietDB))
	g.wake(func() { t.Error("onNoRequest called") })
	g.untilListening()
	g.say(noise(g.r, time.Second, quietDB)) // the user pauses
	req := tone(time.Second, speechDB)
	req[0] = 12345 // marks the request's first sample
	g.say(req)
	g.say(noise(g.r, time.Second, quietDB))
	g.until("reply", func() bool { return len(g.tts.said()) == 1 })
	r := g.llm.requests()[0]
	audio := r.Contents[len(r.Contents)-1].Parts[0].Audio
	// The start came from the live audio: the normal pre-roll of the pause,
	// the whole request, the hangover; nothing of the trigger.
	if d := audioLen(r); !near(d, 1450*time.Millisecond, 60*time.Millisecond) {
		t.Errorf("audio %v, want 1.45 s of the request", d)
	}
	if !slices.Contains(audio, 12345) {
		t.Error("the request's start is missing")
	}
}

func TestBargeInIsNotSeeded(t *testing.T) {
	g := newRig(t, reply("Ставлю."), reply("Хорошо."))
	g.player.hold = make(chan struct{})
	g.wake(nil)
	g.untilListening()
	g.say(g.utterance())
	g.untilState("speaking")
	g.say(tone(400*time.Millisecond, speechDB)) // the reply's echo and the trigger
	g.wake(nil)
	g.untilListening()
	close(g.player.hold)
	g.say(tone(600*time.Millisecond, speechDB))
	g.say(noise(g.r, time.Second, quietDB))
	g.until("second reply", func() bool { return len(g.tts.said()) == 2 })
	// Only the live 600 ms and the hangover; a seed would add 480 ms.
	if d := audioLen(g.llm.requests()[1]); !near(d, 750*time.Millisecond, 60*time.Millisecond) {
		t.Errorf("barge-in audio %v: the seed leaked in", d)
	}
}

func TestHushWhileSpeaking(t *testing.T) {
	g := newRig(t, reply("Ставлю."), reply("Хорошо."))
	g.player.hold = make(chan struct{})
	g.wake(nil)
	g.untilListening()
	g.say(g.utterance())
	g.untilState("speaking")
	if !g.s.SpokeAt().IsZero() {
		t.Errorf("SpokeAt %v while speaking", g.s.SpokeAt())
	}
	g.s.Hush()
	if st := g.s.State(); st != "idle" {
		t.Errorf("state %s right after Hush, want idle", st)
	}
	if g.s.SpokeAt().IsZero() {
		t.Error("SpokeAt not set when the reply stopped")
	}
	if g.s.Busy() {
		t.Error("busy after Hush")
	}
	g.until("playback cancelled", func() bool { _, c := g.player.counts(); return c == 1 })
	if d := g.duckLog(); len(d) == 0 || d[len(d)-1] != 1 {
		t.Errorf("ducks %v, want the music restored", d)
	}
	if ev := g.eventLog(); len(ev) < 2 || ev[len(ev)-2] != "duck 1" || ev[len(ev)-1] != "led off" {
		t.Errorf("events %v, want duck 1 then led off last", ev)
	}
	// The next wake starts without the cancelled conversation.
	g.wake(nil)
	g.untilListening()
	g.say(g.utterance())
	g.until("second reply", func() bool { return len(g.tts.said()) == 2 })
	if reqs := g.llm.requests(); len(reqs) != 2 || len(reqs[1].Contents) != 1 {
		t.Errorf("a wake after Hush should start without history")
	}
}

func TestHushIdle(t *testing.T) {
	g := newRig(t)
	g.s.Hush()
	if ev := g.eventLog(); len(ev) != 0 {
		t.Errorf("events %v from Hush while idle", ev)
	}
	if st := g.s.State(); st != "idle" {
		t.Errorf("state %s, want idle", st)
	}
}

func TestHushThenStaleRun(t *testing.T) {
	g := newRig(t, reply("Ставлю."))
	g.player.hold = make(chan struct{})
	g.wake(nil)
	g.untilListening()
	g.say(g.utterance())
	g.untilState("speaking")
	g.s.Hush()
	before := g.eventLog()
	close(g.player.hold) // the cancelled run winds down
	g.s.wait()
	if ev := g.eventLog(); !slices.Equal(ev, before) {
		t.Errorf("events %v after the cancelled run ended, were %v", ev, before)
	}
	if st := g.s.State(); st != "idle" {
		t.Errorf("state %s, want idle", st)
	}
}

// A stale run inside a Duck callback must not land its event after Hush's.
func TestHushOrdersAfterStaleDuck(t *testing.T) {
	g := newRig(t)
	entered, release := make(chan struct{}), make(chan struct{})
	g.duckGate = func(f float64) {
		if f == duckGain {
			close(entered)
			<-release
		}
	}
	g.wake(nil)
	<-entered // the run is between its gen check and its Duck call
	hushed := make(chan struct{})
	go func() { g.s.Hush(); close(hushed) }()
	select {
	case <-hushed: // ordering is already broken; release anyway
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-hushed
	g.s.wait()
	ev := g.eventLog()
	if want := []string{"duck 0.2", "duck 1", "led off"}; !slices.Equal(ev, want) {
		t.Errorf("events %v, want %v", ev, want)
	}
}

func TestHushDropsNoRequestCallback(t *testing.T) {
	g := newRig(t)
	called := make(chan struct{}, 1)
	g.wake(func() { called <- struct{}{} })
	g.untilListening()
	g.s.Hush()
	g.s.wait()
	select {
	case <-called:
		t.Error("onNoRequest called for a hushed run")
	default:
	}
}
