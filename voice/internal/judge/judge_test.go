package judge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"voice/internal/wav"
)

func fakeGemini(t *testing.T, status int, reply string, got *map[string]any) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models/m1:generateContent" || r.Header.Get("x-goog-api-key") != "k" {
			t.Errorf("request %s key %q", r.URL.Path, r.Header.Get("x-goog-api-key"))
		}
		b, _ := io.ReadAll(r.Body)
		if got != nil {
			json.Unmarshal(b, got)
		}
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	c := New("k")
	c.URL, c.Model = srv.URL, "m1"
	return c
}

func answer(text string) string {
	b, _ := json.Marshal(map[string]any{"candidates": []any{map[string]any{
		"content": map[string]any{"parts": []any{map[string]any{"text": text}}},
	}}})
	return string(b)
}

func TestJudgeSendsClipAndParsesVerdict(t *testing.T) {
	var req map[string]any
	c := fakeGemini(t, 200, answer(`{"command":false,"confidence":0.9,"heard":"стоп машина","reason":"это строчка песни"}`), &req)
	audio := make([]int16, 16000)
	audio[5] = 1234
	v, err := c.Judge(context.Background(), Hit{Word: "стоп", Action: "pause", Audio: audio, Rate: 16000, Undone: 3, Playback: true})
	if err != nil {
		t.Fatal(err)
	}
	if v.Command || v.Confidence != 0.9 || v.Heard != "стоп машина" || v.Reason == "" {
		t.Fatalf("verdict %+v", v)
	}
	parts := req["contents"].([]any)[0].(map[string]any)["parts"].([]any)
	text := parts[0].(map[string]any)["text"].(string)
	for _, want := range []string{"«стоп»", "pause", "Music was playing", "took the action back 3 s"} {
		if !strings.Contains(text, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	inline := parts[1].(map[string]any)["inline_data"].(map[string]any)
	if inline["mime_type"] != "audio/wav" {
		t.Fatalf("mime %v", inline["mime_type"])
	}
	b, _ := base64.StdEncoding.DecodeString(inline["data"].(string))
	pcm, rate, err := wav.Decode(b)
	if err != nil || rate != 16000 || len(pcm) != 16000 || pcm[5] != 1234 {
		t.Fatalf("clip: rate %d len %d err %v", rate, len(pcm), err)
	}
	gc := req["generationConfig"].(map[string]any)
	if gc["responseMimeType"] != "application/json" || gc["responseSchema"] == nil {
		t.Fatalf("generationConfig %v", gc)
	}
}

func TestJudgeAPIError(t *testing.T) {
	c := fakeGemini(t, 400, `{"error":{"code":400,"message":"User location is not supported for the API use.","status":"FAILED_PRECONDITION"}}`, nil)
	_, err := c.Judge(context.Background(), Hit{Word: "стоп", Rate: 16000})
	if err == nil || !strings.Contains(err.Error(), "location is not supported") {
		t.Fatalf("err %v", err)
	}
}

func TestJudgeRejectsNonJSONAnswer(t *testing.T) {
	c := fakeGemini(t, 200, answer("да, это команда"), nil)
	if _, err := c.Judge(context.Background(), Hit{Word: "стоп", Rate: 16000}); err == nil {
		t.Fatal("no error for a prose answer")
	}
}

func TestJudgeEmptyAnswer(t *testing.T) {
	c := fakeGemini(t, 200, `{"candidates":[{"finishReason":"SAFETY"}]}`, nil)
	_, err := c.Judge(context.Background(), Hit{Word: "стоп", Rate: 16000})
	if err == nil || !strings.Contains(err.Error(), "SAFETY") {
		t.Fatalf("err %v", err)
	}
}

func TestReadKey(t *testing.T) {
	dir := t.TempDir()
	if k, err := ReadKey(filepath.Join(dir, "none")); k != "" || err != nil {
		t.Fatalf("missing key: %q %v", k, err)
	}
	p := filepath.Join(dir, "gemini.key")
	os.WriteFile(p, []byte("  abc\n"), 0o600)
	if k, err := ReadKey(p); k != "abc" || err != nil {
		t.Fatalf("key %q %v", k, err)
	}
}
