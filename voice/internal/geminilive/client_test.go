package geminilive

import (
	"context"
	"encoding/json"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeServer accepts one connection, hands the raw setup to the test, answers
// setupComplete and then runs script with the live connection.
func fakeServer(t *testing.T, setupCh chan<- json.RawMessage, script func(ctx context.Context, c *websocket.Conn)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") != "secret" {
			t.Error("missing authentication")
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		var msg map[string]json.RawMessage
		if err = wsjson.Read(ctx, c, &msg); err != nil {
			t.Error(err)
			return
		}
		if setupCh != nil {
			setupCh <- msg["setup"]
		}
		wsjson.Write(ctx, c, map[string]any{"setupComplete": map[string]any{}})
		if script != nil {
			script(ctx, c)
		}
	}))
}

func wsURL(srv *httptest.Server) string { return "ws" + strings.TrimPrefix(srv.URL, "http") }

func TestSetupStreaming(t *testing.T) {
	setupCh := make(chan json.RawMessage, 1)
	srv := fakeServer(t, setupCh, nil)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, Config{Key: "secret", URL: wsURL(srv), Voice: "Kore", Tools: []Declaration{{Name: "pause", Parameters: map[string]any{"type": "OBJECT"}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	raw := <-setupCh
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
		SessionResumption        *struct{ Handle *string }
		ContextWindowCompression *struct{ SlidingWindow *struct{} }
	}
	if err = json.Unmarshal(raw, &setup); err != nil {
		t.Fatal(err)
	}
	if setup.Model != "models/gemini-3.8-live" || len(setup.GenerationConfig.ResponseModalities) != 1 || setup.GenerationConfig.ResponseModalities[0] != "AUDIO" || setup.GenerationConfig.SpeechConfig.VoiceConfig.PrebuiltVoiceConfig.VoiceName != "Kore" || len(setup.Tools) != 1 || setup.InputAudioTranscription == nil || setup.OutputAudioTranscription == nil {
		t.Errorf("bad setup: %s", raw)
	}
	if setup.SessionResumption == nil || setup.SessionResumption.Handle != nil {
		t.Errorf("sessionResumption must be present without handle: %s", raw)
	}
	if setup.ContextWindowCompression == nil || setup.ContextWindowCompression.SlidingWindow == nil {
		t.Errorf("missing contextWindowCompression.slidingWindow: %s", raw)
	}
	if strings.Contains(string(raw), "automaticActivityDetection") {
		t.Errorf("server VAD must stay at its default (enabled): %s", raw)
	}
	if strings.Contains(string(raw), "thinkingConfig") || strings.Contains(string(raw), "proactivity") {
		t.Error("unsupported legacy config")
	}
}

func TestSetupResumeHandle(t *testing.T) {
	setupCh := make(chan json.RawMessage, 1)
	srv := fakeServer(t, setupCh, nil)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, Config{Key: "secret", URL: wsURL(srv), Handle: "h1"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var setup struct{ SessionResumption struct{ Handle string } }
	if err = json.Unmarshal(<-setupCh, &setup); err != nil || setup.SessionResumption.Handle != "h1" {
		t.Fatalf("handle %q %v", setup.SessionResumption.Handle, err)
	}
}

func TestStreamAudio(t *testing.T) {
	got := make(chan string, 3)
	srv := fakeServer(t, nil, func(ctx context.Context, c *websocket.Conn) {
		for i := 0; i < 3; i++ {
			var msg map[string]json.RawMessage
			if err := wsjson.Read(ctx, c, &msg); err != nil {
				t.Error(err)
				return
			}
			got <- string(msg["realtimeInput"])
		}
	})
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, Config{Key: "secret", URL: wsURL(srv)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < 3; i++ {
		if err = c.StreamAudio(ctx, []int16{1, -1}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		var in string
		select {
		case in = <-got:
		case <-ctx.Done():
			t.Fatal("timeout")
		}
		if strings.Contains(in, "activity") {
			t.Errorf("unexpected activity marker: %s", in)
		}
		var audio struct {
			Audio struct{ MimeType, Data string }
		}
		json.Unmarshal([]byte(in), &audio)
		if audio.Audio.MimeType != "audio/pcm;rate=16000" || audio.Audio.Data != "AQD//w==" {
			t.Errorf("audio %s", in)
		}
	}
}

func TestResumptionUpdateParsed(t *testing.T) {
	srv := fakeServer(t, nil, func(ctx context.Context, c *websocket.Conn) {
		wsjson.Write(ctx, c, map[string]any{"sessionResumptionUpdate": map[string]any{"newHandle": "h2", "resumable": true}})
		wsjson.Write(ctx, c, map[string]any{"goAway": map[string]any{"timeLeft": "5s"}})
		wsjson.Write(ctx, c, map[string]any{"toolCallCancellation": map[string]any{"ids": []string{"c1"}}})
	})
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, Config{Key: "secret", URL: wsURL(srv)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	m, err := c.Receive(ctx)
	if err != nil || m.SessionResumptionUpdate == nil || m.SessionResumptionUpdate.NewHandle != "h2" || !m.SessionResumptionUpdate.Resumable {
		t.Fatalf("update %+v %v", m, err)
	}
	m, err = c.Receive(ctx)
	if err != nil || m.GoAway == nil || m.GoAway.TimeLeft != "5s" {
		t.Fatalf("goAway %+v %v", m, err)
	}
	m, err = c.Receive(ctx)
	if err != nil || m.ToolCallCancellation == nil || m.ToolCallCancellation.IDs[0] != "c1" {
		t.Fatalf("cancellation %+v %v", m, err)
	}
}

func TestConcurrentWrites(t *testing.T) {
	const n = 50
	var audio, resp atomic.Int32
	done := make(chan struct{})
	srv := fakeServer(t, nil, func(ctx context.Context, c *websocket.Conn) {
		defer close(done)
		for i := 0; i < 2*n; i++ {
			var msg map[string]json.RawMessage
			if err := wsjson.Read(ctx, c, &msg); err != nil {
				t.Error(err)
				return
			}
			if _, ok := msg["realtimeInput"]; ok {
				audio.Add(1)
			}
			if _, ok := msg["toolResponse"]; ok {
				resp.Add(1)
			}
		}
	})
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, Config{Key: "secret", URL: wsURL(srv)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			if err := c.StreamAudio(ctx, []int16{1}); err != nil {
				t.Error(err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			if err := c.Respond(ctx, []FunctionResponse{{ID: "c", Name: "x", Response: map[string]any{}}}); err != nil {
				t.Error(err)
			}
		}
	}()
	wg.Wait()
	<-done
	if audio.Load() != n || resp.Load() != n {
		t.Fatalf("audio=%d responses=%d", audio.Load(), resp.Load())
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
