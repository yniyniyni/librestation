package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// sse serves chunks as an SSE stream and hands the request body back through
// got.
func sse(t *testing.T, got *string, chunks ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if got != nil {
			*got = string(b)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			w.(http.Flusher).Flush()
		}
	}))
}

func client(srv *httptest.Server) *Client {
	c := NewClient("k")
	c.URL = srv.URL
	return c
}

func userText(s string) []Content {
	return []Content{{Role: "user", Parts: []Part{{Text: s}}}}
}

func TestStreamMergesTextDeltas(t *testing.T) {
	srv := sse(t, nil,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"При"}]}}]}`,
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"вет."}]}}]}`)
	defer srv.Close()
	var deltas []string
	c, err := client(srv).Stream(context.Background(), Request{Contents: userText("hi")}, func(s string) { deltas = append(deltas, s) })
	if err != nil {
		t.Fatal(err)
	}
	if len(deltas) != 2 || deltas[0] != "При" || deltas[1] != "вет." {
		t.Fatalf("deltas = %q", deltas)
	}
	if c.Role != "model" || len(c.Parts) != 1 || c.Parts[0].Text != "Привет." {
		t.Fatalf("content = %+v", c)
	}
}

func TestStreamReturnsFunctionCall(t *testing.T) {
	srv := sse(t, nil, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"play_music","args":{"query":"the weeknd"}}}]}}]}`)
	defer srv.Close()
	c, err := client(srv).Stream(context.Background(), Request{Contents: userText("play")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Parts) != 1 || c.Parts[0].Call == nil || c.Parts[0].Call.Name != "play_music" || c.Parts[0].Call.Args["query"] != "the weeknd" {
		t.Fatalf("content = %+v", c)
	}
}

func TestStreamKeepsThoughtSignature(t *testing.T) {
	part := `{"functionCall":{"name":"pause","args":{}},"thoughtSignature":"abc"}`
	var got string
	srv := sse(t, &got, `{"candidates":[{"content":{"role":"model","parts":[`+part+`]}}]}`)
	defer srv.Close()
	cl := client(srv)
	c, err := cl.Stream(context.Background(), Request{Contents: userText("stop")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	next := append(userText("stop"), c, Content{Role: "user", Parts: []Part{{Result: &Result{Name: "pause", Response: map[string]any{"ok": true}}}}})
	if _, err := cl.Stream(context.Background(), Request{Contents: next}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, part) {
		t.Fatalf("part not sent back verbatim: %s", got)
	}
	if !strings.Contains(got, `"functionResponse"`) {
		t.Fatalf("no functionResponse: %s", got)
	}
}

func TestStreamSendsAudioAsWav(t *testing.T) {
	var got, key, query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got, key, query = string(b), r.Header.Get("x-goog-api-key"), r.URL.RawQuery
		fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]}}]}\n\n")
	}))
	defer srv.Close()
	req := Request{
		System:       "sys",
		Contents:     []Content{{Role: "user", Parts: []Part{{Audio: []int16{1, 2, 3}, AudioRate: 16000}}}},
		Funcs:        []FuncDecl{{Name: "f", Description: "d", Params: map[string]any{"type": "object"}}},
		GoogleSearch: true,
	}
	if _, err := client(srv).Stream(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	if key != "k" || strings.Contains(query, "key") {
		t.Fatalf("key=%q query=%q", key, query)
	}
	for _, want := range []string{`"inline_data"`, `"audio/wav"`, `"functionDeclarations"`, `"google_search":{}`, `"systemInstruction"`} {
		if !strings.Contains(got, want) {
			t.Errorf("body lacks %s: %s", want, got)
		}
	}
}

func TestStreamAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":{"status":"FAILED_PRECONDITION","message":"User location is not supported"}}`)
	}))
	defer srv.Close()
	_, err := client(srv).Stream(context.Background(), Request{Contents: userText("hi")}, nil)
	if err == nil || !strings.Contains(err.Error(), "FAILED_PRECONDITION") || !strings.Contains(err.Error(), "User location is not supported") {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamBadModel(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { n.Add(1) }))
	defer srv.Close()
	_, err := client(srv).Stream(context.Background(), Request{Model: "x y", Contents: userText("hi")}, nil)
	if err == nil || n.Load() != 0 {
		t.Fatalf("err=%v requests=%d", err, n.Load())
	}
}

func TestStreamEmptyAnswerIsTyped(t *testing.T) {
	srv := sse(t, nil, `{"candidates":[{"content":{"role":"model"},"finishReason":"STOP"}]}`)
	defer srv.Close()
	_, err := client(srv).Stream(context.Background(), Request{Contents: userText("hi")}, nil)
	var ee *EmptyAnswerError
	if !errors.As(err, &ee) || ee.Reason != "STOP" || !strings.Contains(err.Error(), "empty answer") {
		t.Fatalf("err = %v", err)
	}
}
