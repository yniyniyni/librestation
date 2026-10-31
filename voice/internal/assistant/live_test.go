package assistant

import (
	"context"
	"errors"
	"testing"

	"voice/internal/agent"
	"voice/internal/geminilive"
)

type fakeLiveSession struct {
	messages  []geminilive.Message
	responses [][]geminilive.FunctionResponse
	closed    bool
}

func (f *fakeLiveSession) SendAudio(context.Context, []int16) error { return nil }
func (f *fakeLiveSession) Receive(context.Context) (geminilive.Message, error) {
	if len(f.messages) == 0 {
		return geminilive.Message{}, errors.New("fake live: no more messages")
	}
	m := f.messages[0]
	f.messages = f.messages[1:]
	return m, nil
}
func (f *fakeLiveSession) Respond(_ context.Context, responses []geminilive.FunctionResponse) error {
	f.responses = append(f.responses, responses)
	return nil
}
func (f *fakeLiveSession) Close() { f.closed = true }

func TestLiveTurnEndConversationStopsSession(t *testing.T) {
	live := &fakeLiveSession{messages: []geminilive.Message{{
		ToolCall: &struct {
			FunctionCalls []geminilive.FunctionCall `json:"functionCalls"`
		}{FunctionCalls: []geminilive.FunctionCall{{ID: "end-1", Name: "end_conversation"}}},
	}}}
	s := New(Deps{
		Agent: &agent.Agent{},
		Live:  func(context.Context) (LiveSession, error) { return live, nil },
	})
	s.gen = 1

	err := s.liveTurn(context.Background(), 1, []int16{1, 2})
	if !errors.Is(err, errLiveDone) {
		t.Fatalf("liveTurn error = %v, want %v", err, errLiveDone)
	}
	if got := s.Status().State; got != "idle" {
		t.Fatalf("state = %q, want idle", got)
	}
	if len(live.responses) != 1 || len(live.responses[0]) != 1 {
		t.Fatalf("responses = %#v, want one end_conversation response", live.responses)
	}
	response := live.responses[0][0]
	if response.ID != "end-1" || response.Name != "end_conversation" || !response.Response["ok"].(bool) {
		t.Fatalf("response = %#v", response)
	}
	if !live.closed {
		t.Fatal("Live session was not closed")
	}
}
