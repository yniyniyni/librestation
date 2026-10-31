package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"voice/internal/agent"
	"voice/internal/geminilive"
)

// fakeLive is one Live connection driven by the test. Receive ignores its
// ctx on purpose: only Close unblocks it, so a session that forgets to close
// the connection hangs the test.
type fakeLive struct {
	in     chan fakeEvent
	closed chan struct{}
	once   sync.Once

	mu    sync.Mutex
	sent  int // samples streamed
	resps []geminilive.FunctionResponse
}

type fakeEvent struct {
	m   geminilive.Message
	err error
}

func newFakeLive() *fakeLive {
	return &fakeLive{in: make(chan fakeEvent), closed: make(chan struct{})}
}

func (f *fakeLive) StreamAudio(_ context.Context, pcm []int16) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent += len(pcm)
	return nil
}

func (f *fakeLive) Receive(context.Context) (geminilive.Message, error) {
	select {
	case e := <-f.in:
		return e.m, e.err
	case <-f.closed:
		return geminilive.Message{}, errors.New("fake live: closed")
	}
}

func (f *fakeLive) Respond(_ context.Context, r []geminilive.FunctionResponse) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resps = append(f.resps, r...)
	return nil
}

func (f *fakeLive) Close() { f.once.Do(func() { close(f.closed) }) }

func (f *fakeLive) isClosed() bool {
	select {
	case <-f.closed:
		return true
	default:
		return false
	}
}

func (f *fakeLive) streamed() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent
}

func (f *fakeLive) responses() []geminilive.FunctionResponse {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.resps)
}

// fakeOut is a livePlayer; Play (stock phrases) goes to the rig's player.
type fakeOut struct {
	*player
	lmu      sync.Mutex
	chunks   int
	finishes int
	flushes  int
}

func (o *fakeOut) PlayLive(context.Context, []int16, int) error {
	o.lmu.Lock()
	defer o.lmu.Unlock()
	o.chunks++
	return nil
}

func (o *fakeOut) FinishLive(context.Context) error {
	o.lmu.Lock()
	defer o.lmu.Unlock()
	o.finishes++
	return nil
}

func (o *fakeOut) Flush() {
	o.lmu.Lock()
	defer o.lmu.Unlock()
	o.flushes++
}

func (o *fakeOut) counts() (chunks, finishes, flushes int) {
	o.lmu.Lock()
	defer o.lmu.Unlock()
	return o.chunks, o.finishes, o.flushes
}

// liveRig is a rig whose session goes through Live.
type liveRig struct {
	*rig
	out *fakeOut

	dmu     sync.Mutex
	conns   []*fakeLive // handed out by Live, in order
	handles []string    // what each Live call was given
	dialErr error
	base    int // goroutines before the conversation
}

func newLiveRig(t *testing.T, tools ...agent.Tool) *liveRig {
	g := &liveRig{rig: newRig(t)}
	g.out = &fakeOut{player: g.player}
	g.s.d.Player = g.out
	g.s.d.Agent.Tools = tools
	g.s.d.Live = func(_ context.Context, handle string) (LiveSession, error) {
		g.dmu.Lock()
		defer g.dmu.Unlock()
		g.handles = append(g.handles, handle)
		if g.dialErr != nil {
			return nil, g.dialErr
		}
		if len(g.conns) == 0 {
			return nil, errors.New("fake live: no connection")
		}
		f := g.conns[0]
		g.conns = g.conns[1:]
		return f, nil
	}
	g.base = runtime.NumGoroutine()
	return g
}

// conn queues a connection for the next Live call.
func (g *liveRig) conn() *fakeLive {
	f := newFakeLive()
	g.dmu.Lock()
	g.conns = append(g.conns, f)
	g.dmu.Unlock()
	return f
}

func (g *liveRig) dials() []string {
	g.dmu.Lock()
	defer g.dmu.Unlock()
	return slices.Clone(g.handles)
}

// open wakes the session and waits for its Live connection.
func (g *liveRig) open() {
	g.t.Helper()
	g.wake(nil)
	g.until("Live dial", func() bool { return len(g.dials()) == 1 })
}

// send hands msgs to the session's receiver, in order.
func (g *liveRig) send(f *fakeLive, msgs ...geminilive.Message) {
	g.t.Helper()
	for _, m := range msgs {
		select {
		case f.in <- fakeEvent{m: m}:
		case <-time.After(5 * time.Second):
			g.t.Fatalf("message not received (state %s)", g.s.State())
		}
	}
}

// speak feeds channel 0 the way the daemon does, 20 ms blocks.
func (g *liveRig) speak(pcm []int16) {
	for i := 0; i < len(pcm); i += 320 {
		g.s.LiveAudio(pcm[i:min(i+320, len(pcm))])
	}
}

// ended waits for the conversation to end and for every goroutine it
// started to exit.
func (g *liveRig) ended() {
	g.t.Helper()
	g.untilState("idle")
	stopped := make(chan struct{})
	go func() {
		g.s.wait()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		g.t.Fatal("the conversation did not stop")
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > g.base {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			g.t.Fatalf("goroutines leaked: %d > %d\n%s", runtime.NumGoroutine(), g.base, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(time.Millisecond)
	}
}

func (g *liveRig) chunks() int   { c, _, _ := g.out.counts(); return c }
func (g *liveRig) finishes() int { _, f, _ := g.out.counts(); return f }
func (g *liveRig) flushes() int  { _, _, f := g.out.counts(); return f }
func userSpeech() []int16        { return tone(400*time.Millisecond, -30) }
func loudSpeech() []int16        { return tone(300*time.Millisecond, -15) }
func sc(c geminilive.ServerContent) geminilive.Message {
	return geminilive.Message{ServerContent: &c}
}

func mHeard(text string) geminilive.Message {
	return sc(geminilive.ServerContent{InputTranscription: &geminilive.Transcription{Text: text}})
}

func mSaid(text string) geminilive.Message {
	return sc(geminilive.ServerContent{OutputTranscription: &geminilive.Transcription{Text: text}})
}

func mAudio() geminilive.Message {
	blob := &geminilive.Blob{MimeType: "audio/pcm;rate=24000", Data: make([]byte, 2*2400)}
	return sc(geminilive.ServerContent{ModelTurn: &geminilive.Content{Parts: []geminilive.Part{{InlineData: blob}}}})
}

func mDone() geminilive.Message        { return sc(geminilive.ServerContent{TurnComplete: true}) }
func mInterrupted() geminilive.Message { return sc(geminilive.ServerContent{Interrupted: true}) }

func mTool(id, name, args string) geminilive.Message {
	var m geminilive.Message
	m.ToolCall = &struct {
		FunctionCalls []geminilive.FunctionCall `json:"functionCalls"`
	}{FunctionCalls: []geminilive.FunctionCall{{ID: id, Name: name, Args: json.RawMessage(args)}}}
	return m
}

func mCancel(ids ...string) geminilive.Message {
	var m geminilive.Message
	m.ToolCallCancellation = &struct {
		IDs []string `json:"ids"`
	}{IDs: ids}
	return m
}

func mHandle(h string) geminilive.Message {
	var m geminilive.Message
	m.SessionResumptionUpdate = &struct {
		NewHandle string `json:"newHandle"`
		Resumable bool   `json:"resumable"`
	}{NewHandle: h, Resumable: true}
	return m
}

func mGoAway() geminilive.Message {
	var m geminilive.Message
	m.GoAway = &struct {
		TimeLeft string `json:"timeLeft"`
	}{TimeLeft: "10s"}
	return m
}

// liveTool answers {"ok": true}, or err; with block it waits for its ctx.
type liveTool struct {
	name    string
	err     error
	block   bool
	stopped chan error // gets the ctx error of a blocked run
}

func (l liveTool) Decl() agent.FuncDecl { return agent.FuncDecl{Name: l.name} }

func (l liveTool) Run(ctx context.Context, _ map[string]any) (map[string]any, error) {
	if l.block {
		<-ctx.Done()
		l.stopped <- ctx.Err()
		return nil, ctx.Err()
	}
	if l.err != nil {
		return nil, l.err
	}
	return map[string]any{"ok": true}, nil
}

// reply plays one answer with no tools and waits for the follow-up window.
func (g *liveRig) reply(f *fakeLive, heard, said string) {
	g.t.Helper()
	n := g.finishes() + 1
	g.send(f, mHeard(heard), mAudio(), mSaid(said), mDone())
	g.until("reply played", func() bool { return g.finishes() >= n && g.s.State() == "followup" })
}

func TestLiveOneSessionPerConversation(t *testing.T) {
	g := newLiveRig(t)
	f := g.conn()
	g.speak(noise(g.r, time.Second, quietDB)) // before the wake: the pre-roll
	g.open()
	g.until("pre-roll sent", func() bool { return f.streamed() >= seedLen })

	g.reply(f, "привет", "Привет!")
	g.speak(userSpeech())
	g.reply(f, "как дела", "Хорошо.")

	if d := g.dials(); len(d) != 1 {
		t.Fatalf("Live dialled %d times (%q), want once", len(d), d)
	}
	if f.isClosed() {
		t.Fatal("session closed between turns")
	}
	if c := g.chunks(); c != 2 {
		t.Errorf("played %d chunks, want 2", c)
	}
}

func TestLiveClosesAfterAction(t *testing.T) {
	g := newLiveRig(t, liveTool{name: "play_music"})
	f := g.conn()
	g.open()
	g.send(f, mHeard("включи музыку"), mTool("c1", "play_music", `{"query":"jazz"}`))
	g.until("tool answered", func() bool { return len(f.responses()) == 1 })
	if r := f.responses()[0]; r.ID != "c1" || r.Response["ok"] != true {
		t.Fatalf("response = %#v", r)
	}
	g.send(f, mAudio(), mSaid("Включаю."), mDone())
	g.ended()
	if !f.isClosed() {
		t.Fatal("Live not closed")
	}
	if g.finishes() != 1 {
		t.Errorf("FinishLive %d times, want 1", g.finishes())
	}
	if d := g.duckLog(); !slices.Equal(d, []float64{duckGain, 1}) {
		t.Errorf("ducks %v, want [%v 1]", d, duckGain)
	}
	if st := g.ledStates(); st[len(st)-1] != "off" {
		t.Errorf("ring %v, want it off at the end", st)
	}
}

func TestLiveInfoWindow(t *testing.T) {
	g := newLiveRig(t, liveTool{name: "get_weather"})
	f := g.conn()
	g.open()
	g.send(f, mHeard("какая погода"), mTool("w", "get_weather", `{}`))
	g.until("tool answered", func() bool { return len(f.responses()) == 1 })
	g.send(f, mAudio(), mSaid("Плюс пять."), mDone())
	g.untilState("followup")
	g.clock.Advance(infoWindow - time.Millisecond)
	g.send(f, mHandle("still-open")) // the session still takes messages
	g.clock.Advance(time.Millisecond)
	g.ended()
	if !f.isClosed() {
		t.Fatal("Live not closed")
	}
}

func TestLiveDialogWindowResetBySpeech(t *testing.T) {
	g := newLiveRig(t)
	f := g.conn()
	g.open()
	g.reply(f, "расскажи", "Жил-был кот.")
	g.clock.Advance(5 * time.Second)
	g.speak(userSpeech())
	g.untilState("listening")
	g.clock.Advance(3 * time.Second) // 8 s after the reply: the old window is gone
	g.reply(f, "а дальше", "Конец.")
	if d := g.dials(); len(d) != 1 {
		t.Fatalf("Live dialled %d times, want once", len(d))
	}
	if f.isClosed() {
		t.Fatal("session closed")
	}
	g.clock.Advance(dialogWindow)
	g.ended()
}

func TestLiveBargeIn(t *testing.T) {
	g := newLiveRig(t)
	f := g.conn()
	g.open()
	g.send(f, mHeard("расскажи"), mAudio())
	g.untilState("speaking")
	g.speak(loudSpeech())
	g.until("flush", func() bool { return g.flushes() == 1 })
	g.untilState("listening")
	// The rest of the cut reply is dropped, up to its end.
	g.send(f, mAudio(), mAudio(), mDone())
	g.reply(f, "стоп, другое", "Хорошо.")
	if c := g.chunks(); c != 2 {
		t.Errorf("played %d chunks, want 2 (the cut reply's tail dropped)", c)
	}
	if n := g.finishes(); n != 1 {
		t.Errorf("FinishLive %d times, want 1 (the cut reply never finishes)", n)
	}
}

func TestLiveQuietSpeechDoesNotBargeIn(t *testing.T) {
	g := newLiveRig(t)
	f := g.conn()
	g.open()
	g.send(f, mHeard("расскажи"), mAudio())
	g.untilState("speaking")
	g.speak(tone(time.Second, bargeInDBFS-6)) // the reply's echo in channel 0
	g.send(f, mAudio(), mDone())
	g.untilState("followup")
	if n := g.flushes(); n != 0 {
		t.Errorf("flushed %d times, want 0", n)
	}
}

func TestLiveServerInterrupted(t *testing.T) {
	g := newLiveRig(t)
	f := g.conn()
	g.open()
	g.send(f, mHeard("расскажи"), mAudio())
	g.untilState("speaking")
	g.send(f, mInterrupted())
	g.until("flush", func() bool { return g.flushes() == 1 })
	g.untilState("listening")
	g.reply(f, "другое", "Хорошо.")
	if c := g.chunks(); c != 2 {
		t.Errorf("played %d chunks, want 2", c)
	}
}

func TestLiveDialFailFallsBackToAgent(t *testing.T) {
	g := newLiveRig(t)
	g.llm.steps = append(g.llm.steps, reply("Ставлю."))
	g.dialErr = errors.New("live: connection failed")
	g.wake(nil)
	g.untilListening()
	g.say(g.utterance())
	g.until("agent reply played", func() bool { p, _ := g.player.counts(); return p == 1 })
	if s := g.tts.said(); !slices.Equal(s, []string{"Ставлю."}) {
		t.Errorf("said %q, want the agent's reply", s)
	}
	if c := g.chunks(); c != 0 {
		t.Errorf("Live chunks %d, want 0", c)
	}
	if d := g.dials(); len(d) != 1 {
		t.Errorf("dialled %d times, want once (no retries)", len(d))
	}
}

func TestLiveGoAwayResumes(t *testing.T) {
	g := newLiveRig(t)
	f1, f2 := g.conn(), g.conn()
	g.open()
	g.send(f1, mHandle("h1"), mHeard("привет"), mAudio(), mGoAway(), mSaid("Привет."), mDone())
	g.until("resumed", func() bool { return len(g.dials()) == 2 })
	g.untilState("followup")
	if d := g.dials(); !slices.Equal(d, []string{"", "h1"}) {
		t.Fatalf("dials %q, want a new session then h1", d)
	}
	if !f1.isClosed() {
		t.Error("old connection not closed")
	}
	g.speak(userSpeech())
	g.until("audio to the new connection", func() bool { return f2.streamed() > 0 })
	g.reply(f2, "ещё", "Да.")
	if f2.isClosed() {
		t.Fatal("resumed session closed")
	}
}

func TestLiveGoAwayWithoutHandleCloses(t *testing.T) {
	g := newLiveRig(t)
	f := g.conn()
	g.open()
	g.send(f, mHeard("привет"), mAudio(), mGoAway(), mSaid("Привет."), mDone())
	g.ended()
	if d := g.dials(); len(d) != 1 {
		t.Errorf("dials %q, want one", d)
	}
}

func TestLiveStall(t *testing.T) {
	g := newLiveRig(t)
	f := g.conn()
	g.open()
	g.send(f, mHeard("какая погода"))
	g.untilState("thinking")
	g.clock.Advance(liveStall)
	g.ended()
	if s := g.stockLog(); !slices.Equal(s, []string{"no_internet"}) {
		t.Errorf("stock %v, want [no_internet]", s)
	}
	if !f.isClosed() {
		t.Error("Live not closed")
	}
	if st := g.ledStates(); st[len(st)-1] != "off" {
		t.Errorf("ring %v, want it off", st)
	}
}

func TestLiveReceiveErrorFails(t *testing.T) {
	g := newLiveRig(t)
	f := g.conn()
	g.open()
	g.reply(f, "привет", "Привет.")
	select {
	case f.in <- fakeEvent{err: errors.New("live: connection reset")}:
	case <-time.After(5 * time.Second):
		t.Fatal("error not received")
	}
	g.ended()
	if s := g.stockLog(); !slices.Equal(s, []string{"something_went_wrong"}) {
		t.Errorf("stock %v, want [something_went_wrong]", s)
	}
	if !f.isClosed() {
		t.Error("Live not closed")
	}
	if e := g.s.Status().LastError; e == "" {
		t.Error("LastError empty")
	}
}

// Nothing is heard after the wake: the window closes the session, and the
// receiver blocked in Receive is let go by Close.
func TestLiveNoSpeechCloses(t *testing.T) {
	g := newLiveRig(t)
	f := g.conn()
	var noReq sync.WaitGroup
	noReq.Add(1)
	g.wake(noReq.Done)
	g.until("Live dial", func() bool { return len(g.dials()) == 1 })
	g.untilState("listening")
	g.clock.Advance(startLimit)
	g.ended()
	noReq.Wait()
	if !f.isClosed() {
		t.Error("Live not closed")
	}
}

func TestLiveHushMidSession(t *testing.T) {
	g := newLiveRig(t)
	f := g.conn()
	g.open()
	g.send(f, mHeard("расскажи"), mAudio())
	g.untilState("speaking")
	g.s.Hush()
	g.ended()
	if !f.isClosed() {
		t.Error("Live not closed")
	}
	if g.flushes() == 0 {
		t.Error("reply not flushed")
	}
	if d := g.duckLog(); !slices.Equal(d, []float64{duckGain, 1}) {
		t.Errorf("ducks %v, want [%v 1]", d, duckGain)
	}
	// The next wake starts a new session.
	f2 := g.conn()
	g.wake(nil)
	g.until("second dial", func() bool { return len(g.dials()) == 2 })
	g.reply(f2, "привет", "Привет.")
}

func TestLiveToolCancelled(t *testing.T) {
	stopped := make(chan error, 1)
	g := newLiveRig(t, liveTool{name: "web_answer", block: true, stopped: stopped})
	f := g.conn()
	g.open()
	g.send(f, mHeard("найди"), mTool("q1", "web_answer", `{"query":"x"}`), mCancel("q1"))
	select {
	case err := <-stopped:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("tool stopped with %v, want Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tool not cancelled")
	}
	g.send(f, mAudio(), mDone())
	g.untilState("followup")
	if r := f.responses(); len(r) != 0 {
		t.Errorf("responses %v, want none for a cancelled call", r)
	}
}

func TestLiveStatusPerTurn(t *testing.T) {
	g := newLiveRig(t)
	f := g.conn()
	g.open()
	g.reply(f, "раз", "Один.")
	if st := g.s.Status(); st.LastHeard != "раз" || st.LastReply != "Один." {
		t.Fatalf("status %+v after turn 1", st)
	}
	g.send(f, mHeard("два"))
	g.until("turn 2 heard", func() bool { return g.s.Status().LastHeard == "два" })
	if r := g.s.Status().LastReply; r != "" {
		t.Errorf("LastReply %q at the start of turn 2, want empty", r)
	}
	g.send(f, mAudio(), mSaid("Второй."), mDone())
	g.untilState("followup")
	if st := g.s.Status(); st.LastHeard != "два" || st.LastReply != "Второй." {
		t.Errorf("status %+v after turn 2", st)
	}
}

func TestLiveAudioNeverBlocks(t *testing.T) {
	g := newLiveRig(t)
	block := make([]int16, 320)
	done := make(chan struct{})
	go func() {
		for range 20 * sampleRate / 320 { // 20 s, more than the queue holds
			g.s.LiveAudio(block)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("LiveAudio blocked")
	}
}
