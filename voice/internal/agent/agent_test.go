package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// script replays canned model answers and records what it was asked.
type script struct {
	answers []Content
	errs    map[int]error // the error for answer i, instead of answers[i]
	reqs    []Request
}

func (s *script) Stream(_ context.Context, r Request, onText func(string)) (Content, error) {
	s.reqs = append(s.reqs, r)
	i := len(s.reqs) - 1
	if err := s.errs[i]; err != nil {
		return Content{}, err
	}
	if i >= len(s.answers) {
		return Content{}, errors.New("script exhausted")
	}
	a := s.answers[i]
	for _, p := range a.Parts {
		if p.Text != "" && onText != nil {
			onText(p.Text)
		}
	}
	return a, nil
}

func call(name string, args map[string]any) Content {
	return Content{Role: "model", Parts: []Part{{Call: &Call{Name: name, Args: args}}}}
}

func reply(s string) Content {
	return Content{Role: "model", Parts: []Part{{Text: s}}}
}

type fakeTool struct {
	name string
	got  map[string]any
	err  error
}

func (f *fakeTool) Decl() FuncDecl { return FuncDecl{Name: f.name, Description: "test"} }
func (f *fakeTool) Run(_ context.Context, args map[string]any) (map[string]any, error) {
	f.got = args
	return map[string]any{"ok": true}, f.err
}

func newAgent(llm Streamer, tools ...Tool) *Agent {
	return &Agent{
		Model: func() string { return "m" },
		LLM:   llm,
		Tools: tools,
		Now:   func() time.Time { return time.Date(2026, 10, 31, 21, 7, 0, 0, time.UTC) },
	}
}

func turn(t *testing.T, a *Agent, hist []Content) ([]Content, Outcome, []string, error) {
	t.Helper()
	var said []string
	h, o, err := a.Turn(context.Background(), hist, []int16{1, 2, 3}, func(s string) { said = append(said, s) })
	return h, o, said, err
}

func TestTurnRunsToolAndSpeaksReply(t *testing.T) {
	tool := &fakeTool{name: "play_music"}
	llm := &script{answers: []Content{call("play_music", map[string]any{"query": "The Weeknd"}), reply("Ставлю The Weeknd.")}}
	a := newAgent(llm, tool)
	_, o, said, err := turn(t, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tool.got["query"] != "The Weeknd" {
		t.Errorf("tool args = %v", tool.got)
	}
	if len(said) != 1 || said[0] != "Ставлю The Weeknd." {
		t.Errorf("said = %q", said)
	}
	if len(o.Calls) != 1 || o.Calls[0] != "play_music" || o.Spoken != "Ставлю The Weeknd." {
		t.Errorf("outcome = %+v", o)
	}
	r := llm.reqs[0]
	if r.Model != "m" || !strings.Contains(r.System, "суббота, 31 октября 2026, 21:07") {
		t.Errorf("request = model %q system %q", r.Model, r.System)
	}
	names := map[string]bool{}
	for _, f := range r.Funcs {
		names[f.Name] = true
	}
	if !names["play_music"] || !names["no_request"] || !names["start_conversation"] {
		t.Errorf("funcs = %v", names)
	}
}

func TestTurnNoRequest(t *testing.T) {
	llm := &script{answers: []Content{call("no_request", nil)}}
	h, o, said, err := turn(t, newAgent(llm), nil)
	if err != nil || !o.NoRequest || len(said) != 0 {
		t.Fatalf("err %v outcome %+v said %q", err, o, said)
	}
	if len(llm.reqs) != 1 {
		t.Errorf("model calls = %d, want 1", len(llm.reqs))
	}
	// Every call in the history must have its response.
	if last := h[len(h)-1]; last.Role != "user" || last.Parts[0].Result == nil {
		t.Errorf("history ends with %+v", last)
	}
}

func TestTurnStartConversation(t *testing.T) {
	llm := &script{answers: []Content{call("start_conversation", nil)}}
	_, o, said, err := turn(t, newAgent(llm), nil)
	if err != nil || !o.StartConversation || len(said) != 0 || len(llm.reqs) != 1 {
		t.Fatalf("err %v outcome %+v said %q calls %d", err, o, said, len(llm.reqs))
	}
}

func TestToolErrorGoesToModel(t *testing.T) {
	tool := &fakeTool{name: "like", err: errors.New("не играет")}
	llm := &script{answers: []Content{call("like", nil), reply("Не вышло.")}}
	if _, _, _, err := turn(t, newAgent(llm, tool), nil); err != nil {
		t.Fatal(err)
	}
	cs := llm.reqs[1].Contents
	res := cs[len(cs)-1].Parts[0].Result
	if res == nil || res.Name != "like" || res.Response["error"] != "не играет" {
		t.Errorf("function response = %+v", res)
	}
}

func TestUnknownToolGoesToModel(t *testing.T) {
	llm := &script{answers: []Content{call("nope", nil), reply("Не умею.")}}
	if _, _, _, err := turn(t, newAgent(llm), nil); err != nil {
		t.Fatal(err)
	}
	cs := llm.reqs[1].Contents
	if res := cs[len(cs)-1].Parts[0].Result; res == nil || res.Response["error"] == nil {
		t.Errorf("function response = %+v", res)
	}
}

func TestToolRoundsCapped(t *testing.T) {
	tool := &fakeTool{name: "t"}
	var answers []Content
	for i := 0; i < 10; i++ {
		answers = append(answers, call("t", nil))
	}
	llm := &script{answers: answers}
	_, _, _, err := turn(t, newAgent(llm, tool), nil)
	if !errors.Is(err, errTooManyRounds) {
		t.Fatalf("err = %v", err)
	}
	if len(llm.reqs) != 4 {
		t.Errorf("model calls = %d, want 4", len(llm.reqs))
	}
}

func TestSpokenReplyCapped(t *testing.T) {
	long := strings.Repeat("Раз два три четыре пять. ", 40) // 1000 characters
	llm := &script{answers: []Content{reply(long)}}
	_, o, said, err := turn(t, newAgent(llm), nil)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, s := range said {
		total += len([]rune(s))
	}
	if total == 0 || total > maxSpoken {
		t.Errorf("spoken %d characters", total)
	}
	if !strings.HasSuffix(said[len(said)-1], ".") || o.Spoken != strings.Join(said, " ") {
		t.Errorf("last %q spoken %q", said[len(said)-1], o.Spoken)
	}
}

func TestSentencesStreamAsTheyComplete(t *testing.T) {
	var order []string
	llm := streamFunc(func(onText func(string)) Content {
		onText("Первое предложение. Вто")
		order = append(order, "after-first-delta")
		onText("рое! Третье")
		onText("…")
		return reply("Первое предложение. Второе! Третье…")
	})
	var said []string
	a := newAgent(llm)
	_, _, err := a.Turn(context.Background(), nil, []int16{1}, func(s string) {
		said = append(said, s)
		order = append(order, s)
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Первое предложение.", "Второе!", "Третье…"}
	if strings.Join(said, "|") != strings.Join(want, "|") {
		t.Errorf("said = %q", said)
	}
	if order[0] != "Первое предложение." {
		t.Errorf("first sentence not spoken before the stream ended: %q", order)
	}
}

type streamFunc func(onText func(string)) Content

func (f streamFunc) Stream(_ context.Context, _ Request, onText func(string)) (Content, error) {
	return f(onText), nil
}

func TestHistoryCarriesOver(t *testing.T) {
	llm := &script{answers: []Content{reply("Привет."), reply("Пока.")}}
	a := newAgent(llm)
	h, _, _, err := turn(t, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := turn(t, a, h); err != nil {
		t.Fatal(err)
	}
	cs := llm.reqs[1].Contents
	if len(cs) != 3 || cs[0].Parts[0].Audio == nil || cs[1].Role != "model" || cs[1].Parts[0].Text != "Привет." || cs[2].Parts[0].Audio == nil {
		t.Errorf("second request contents = %+v", cs)
	}
}

func TestRussianDateTime(t *testing.T) {
	d, c := RussianDateTime(time.Date(2026, 10, 31, 21, 7, 0, 0, time.UTC))
	if d != "суббота, 31 октября 2026" || c != "21:07" {
		t.Errorf("got %q %q", d, c)
	}
}

func TestEmptyAnswerAfterToolSaysDone(t *testing.T) {
	tool := &fakeTool{name: "like"}
	llm := &script{answers: []Content{call("like", nil)}, errs: map[int]error{1: &EmptyAnswerError{Reason: "STOP"}}}
	h, o, said, err := turn(t, newAgent(llm, tool), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(said) != 1 || said[0] != doneReply || o.Spoken != doneReply {
		t.Errorf("said %q outcome %+v", said, o)
	}
	if last := h[len(h)-1]; last.Role != "model" || last.Parts[0].Text != doneReply {
		t.Errorf("history ends with %+v", last)
	}
}

func TestEmptyAnswerAfterSpokenToolRoundSaysNothingMore(t *testing.T) {
	tool := &fakeTool{name: "like"}
	first := Content{Role: "model", Parts: []Part{{Text: "Лайкаю. "}, {Call: &Call{Name: "like"}}}}
	llm := &script{answers: []Content{first}, errs: map[int]error{1: &EmptyAnswerError{Reason: "STOP"}}}
	_, _, said, err := turn(t, newAgent(llm, tool), nil)
	if err != nil || len(said) != 1 || said[0] != "Лайкаю." {
		t.Fatalf("err %v said %q", err, said)
	}
}

func TestEmptyAnswerAfterFailedToolSaysFailed(t *testing.T) {
	tool := &fakeTool{name: "like", err: errors.New("не играет")}
	llm := &script{answers: []Content{call("like", nil)}, errs: map[int]error{1: &EmptyAnswerError{Reason: "STOP"}}}
	h, _, said, err := turn(t, newAgent(llm, tool), nil)
	if err != nil || len(said) != 1 || said[0] != failedReply {
		t.Fatalf("err %v said %q", err, said)
	}
	if last := h[len(h)-1]; last.Role != "model" || last.Parts[0].Text != failedReply {
		t.Errorf("history ends with %+v", last)
	}
}

func TestEmptyAnswerFirstRoundIsError(t *testing.T) {
	llm := &script{errs: map[int]error{0: &EmptyAnswerError{Reason: "STOP"}}}
	if _, _, _, err := turn(t, newAgent(llm), nil); err == nil {
		t.Fatal("no error for an empty first answer")
	}
	llm = &script{answers: []Content{call("like", nil)}, errs: map[int]error{1: &EmptyAnswerError{Reason: "SAFETY"}}}
	if _, _, _, err := turn(t, newAgent(llm, &fakeTool{name: "like"}), nil); err == nil {
		t.Fatal("no error for an empty SAFETY answer")
	}
}
