package tts

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"voice/internal/wav"
)

func piperFixture(t *testing.T, exitAfter string) (*Piper, string) {
	t.Helper()
	dir := t.TempDir()
	one := filepath.Join(dir, "one.wav")
	pcm := make([]int16, 4410) // 0.2 s at 22050 Hz
	for i := range pcm {
		pcm[i] = int16(i%100 - 50)
	}
	if err := wav.WriteFile(one, pcm, 22050); err != nil {
		t.Fatal(err)
	}
	startLog := filepath.Join(dir, "starts")
	out := filepath.Join(dir, "out")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := []string{"sh", "testdata/fakepiper.sh", one, startLog}
	if exitAfter != "" {
		cmd = append(cmd, exitAfter)
	}
	return &Piper{Cmd: cmd, OutDir: out, HostOutDir: out}, startLog
}

func starts(t *testing.T, log string) int {
	b, _ := os.ReadFile(log)
	return strings.Count(string(b), "started")
}

func TestPiperResident(t *testing.T) {
	p, startLog := piperFixture(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		pcm, rate, err := p.Synth(ctx, "привет")
		if err != nil {
			t.Fatal(err)
		}
		if rate != 22050 || len(pcm) != 4410 {
			t.Fatalf("rate %d, %d samples", rate, len(pcm))
		}
	}
	if n := starts(t, startLog); n != 1 {
		t.Errorf("process started %d times, want 1", n)
	}
	if left, _ := os.ReadDir(p.HostOutDir); len(left) != 0 {
		t.Errorf("files left: %v", left)
	}
}

func TestPiperLazyStart(t *testing.T) {
	p, startLog := piperFixture(t, "")
	if _, _, err := p.Synth(context.Background(), "раз"); err != nil {
		t.Fatal(err)
	}
	if n := starts(t, startLog); n != 1 {
		t.Errorf("started %d times", n)
	}
	p.proc.kill()
}

func TestPiperRestartsAfterCrash(t *testing.T) {
	p, startLog := piperFixture(t, "1")
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, _, err := p.Synth(ctx, "раз"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if n := starts(t, startLog); n < 2 {
		t.Errorf("started %d times, want a restart", n)
	}
}

func TestPiperCtxKillsStuckProcess(t *testing.T) {
	p, _ := piperFixture(t, "")
	p.Cmd = []string{"sh", "-c", "while read l; do :; done"} // never answers
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, _, err := p.Synth(ctx, "x"); err == nil {
		t.Fatal("want error")
	}
	if time.Since(start) > 2*time.Second {
		t.Error("Synth did not honour ctx")
	}
	if p.proc != nil {
		t.Error("stuck process not dropped")
	}
}

func pcmB64(s []int16) string {
	b := make([]byte, 2*len(s))
	for i, v := range s {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	return base64.StdEncoding.EncodeToString(b)
}

func TestGeminiTTSDecodes(t *testing.T) {
	want := []int16{0, 1, -1, 32767, -32768}
	var gotKey, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotPath = r.Header.Get("x-goog-api-key"), r.URL.String()
		var sb strings.Builder
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		sb.Write(buf[:n])
		gotBody = sb.String()
		w.Write([]byte(`{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"audio/L16;codec=pcm;rate=24000","data":"` + pcmB64(want) + `"}}]}}]}`))
	}))
	defer srv.Close()
	g := &GeminiTTS{Key: "secret", URL: srv.URL}
	pcm, rate, err := g.Synth(context.Background(), "привет")
	if err != nil {
		t.Fatal(err)
	}
	if rate != 24000 || len(pcm) != len(want) {
		t.Fatalf("rate %d, pcm %v", rate, pcm)
	}
	for i := range want {
		if pcm[i] != want[i] {
			t.Errorf("sample %d: %d != %d", i, pcm[i], want[i])
		}
	}
	if gotKey != "secret" || strings.Contains(gotPath, "secret") {
		t.Errorf("key header %q, url %q", gotKey, gotPath)
	}
	if !strings.Contains(gotPath, DefaultTTSModel) || !strings.Contains(gotBody, `"voiceName":"Kore"`) ||
		!strings.Contains(gotBody, `"AUDIO"`) {
		t.Errorf("path %q body %s", gotPath, gotBody)
	}
}

func TestGeminiTTSError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"status":"RESOURCE_EXHAUSTED","message":"quota"}}`))
	}))
	defer srv.Close()
	_, _, err := (&GeminiTTS{Key: "k", URL: srv.URL}).Synth(context.Background(), "x")
	if err == nil || err.Error() != "gemini: RESOURCE_EXHAUSTED: quota" {
		t.Errorf("err = %v", err)
	}
}

func TestMimeRate(t *testing.T) {
	if mimeRate("audio/L16;codec=pcm;rate=16000") != 16000 || mimeRate("audio/L16") != 24000 {
		t.Error("mimeRate")
	}
}

type stub struct {
	pcm []int16
	err error
}

func (s stub) Synth(context.Context, string) ([]int16, int, error) { return s.pcm, 22050, s.err }

func TestFallbackUsesBackup(t *testing.T) {
	f := Fallback{Primary: stub{err: errors.New("boom")}, Backup: stub{pcm: []int16{7, 8}}}
	pcm, rate, err := f.Synth(context.Background(), "x")
	if err != nil || rate != 22050 || len(pcm) != 2 || pcm[0] != 7 {
		t.Errorf("got %v %d %v", pcm, rate, err)
	}
	f.Primary = stub{pcm: []int16{1}}
	if pcm, _, _ := f.Synth(context.Background(), "x"); pcm[0] != 1 {
		t.Error("primary not preferred")
	}
}
