package geminilive

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// An actual WebSocket server checks our wire contract, not SDK implementation details.
func TestLiveWire(t *testing.T) {
	done := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") != "secret" {
			t.Error("missing authentication")
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			done <- err
			return
		}
		defer c.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		var msg map[string]json.RawMessage
		if err = wsjson.Read(ctx, c, &msg); err != nil {
			done <- err
			return
		}
		var setup struct {
			Model            string
			GenerationConfig struct {
				ResponseModalities []string
				SpeechConfig       struct {
					VoiceConfig struct{ PrebuiltVoiceConfig struct{ VoiceName string } }
				}
			}
			Tools                    []json.RawMessage
			InputAudioTranscription  *struct{}
			OutputAudioTranscription *struct{}
		}
		json.Unmarshal(msg["setup"], &setup)
		if setup.Model != "models/gemini-3.8-live" || len(setup.GenerationConfig.ResponseModalities) != 1 || setup.GenerationConfig.ResponseModalities[0] != "AUDIO" || setup.GenerationConfig.SpeechConfig.VoiceConfig.PrebuiltVoiceConfig.VoiceName != "Kore" || len(setup.Tools) != 1 || setup.InputAudioTranscription == nil || setup.OutputAudioTranscription == nil {
			t.Errorf("bad setup: %s", msg["setup"])
		}
		if strings.Contains(string(msg["setup"]), "thinkingConfig") || strings.Contains(string(msg["setup"]), "proactivity") {
			t.Error("unsupported legacy config")
		}
		wsjson.Write(ctx, c, map[string]any{"setupComplete": map[string]any{}})
		if err = wsjson.Read(ctx, c, &msg); err != nil {
			done <- err
			return
		}
		var audio struct {
			Audio struct {
				MimeType string
				Data     string
			}
		}
		json.Unmarshal(msg["realtimeInput"], &audio)
		if audio.Audio.MimeType != "audio/pcm;rate=16000" || audio.Audio.Data != "AQD//w==" {
			t.Errorf("audio %s", msg["realtimeInput"])
		}
		if err = wsjson.Read(ctx, c, &msg); err != nil {
			done <- err
			return
		}
		var end struct {
			AudioStreamEnd bool
		}
		json.Unmarshal(msg["realtimeInput"], &end)
		if !end.AudioStreamEnd {
			t.Errorf("missing audioStreamEnd: %s", msg["realtimeInput"])
		}
		wsjson.Write(ctx, c, map[string]any{"serverContent": map[string]any{"modelTurn": map[string]any{"parts": []any{map[string]any{"inlineData": map[string]any{"mimeType": "audio/pcm;rate=24000", "data": base64.StdEncoding.EncodeToString([]byte{1, 0})}}}}, "interrupted": true, "turnComplete": true}})
		wsjson.Write(ctx, c, map[string]any{"toolCall": map[string]any{"functionCalls": []any{map[string]any{"id": "c1", "name": "pause", "args": map[string]any{}}}}})
		if err = wsjson.Read(ctx, c, &msg); err != nil {
			done <- err
			return
		}
		var response struct{ FunctionResponses []FunctionResponse }
		json.Unmarshal(msg["toolResponse"], &response)
		if len(response.FunctionResponses) != 1 || response.FunctionResponses[0].ID != "c1" || response.FunctionResponses[0].Name != "pause" {
			t.Errorf("response %s", msg["toolResponse"])
		}
		done <- nil
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, Config{Key: "secret", URL: "ws" + strings.TrimPrefix(srv.URL, "http"), Voice: "Kore", Tools: []Declaration{{Name: "pause", Parameters: map[string]any{"type": "OBJECT"}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err = c.SendAudio(ctx, []int16{1, -1}); err != nil {
		t.Fatal(err)
	}
	e, err := c.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !e.ServerContent.Interrupted || !e.ServerContent.TurnComplete || string(e.ServerContent.ModelTurn.Parts[0].InlineData.Data) != string([]byte{1, 0}) {
		t.Fatalf("event %+v", e)
	}
	e, err = c.Receive(ctx)
	if err != nil || e.ToolCall.FunctionCalls[0].Name != "pause" {
		t.Fatalf("tools %+v %v", e, err)
	}
	if err = c.Respond(ctx, []FunctionResponse{{ID: "c1", Name: "pause", Response: map[string]any{"ok": true}}}); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSetupErrorAndKeyRedaction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := websocket.Accept(w, r, nil)
		defer c.CloseNow()
		var msg any
		wsjson.Read(r.Context(), c, &msg)
		wsjson.Write(r.Context(), c, map[string]any{"error": map[string]any{"code": 400, "message": "bad model"}})
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := Dial(ctx, Config{Key: "private-key", URL: "ws" + strings.TrimPrefix(srv.URL, "http")})
	if err == nil || !strings.Contains(err.Error(), "bad model") {
		t.Fatalf("error %v", err)
	}
	_, err = Dial(ctx, Config{Key: "private-key", URL: "ws://127.0.0.1:1"})
	if err == nil || strings.Contains(err.Error(), "private-key") {
		t.Fatalf("leaks key: %v", err)
	}
}
