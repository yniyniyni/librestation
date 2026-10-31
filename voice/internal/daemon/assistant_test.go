package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"voice/internal/assistant"
	"voice/internal/judge"
	"voice/internal/spotifyapi"
)

type fakeAssistant struct {
	mu     sync.Mutex
	busy   bool
	wakes  int
	blocks int
	noReq  func()
}

func (f *fakeAssistant) Audio([]int16) { f.mu.Lock(); f.blocks++; f.mu.Unlock() }
func (f *fakeAssistant) Wake(_ context.Context, n func()) {
	f.mu.Lock()
	f.wakes++
	f.noReq = n
	f.mu.Unlock()
}
func (f *fakeAssistant) Busy() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.busy }
func (f *fakeAssistant) setBusy(b bool) {
	f.mu.Lock()
	f.busy = b
	f.mu.Unlock()
}

func newAsst(t *testing.T, act *fakeAct, action string) (*Daemon, *fakeAssistant) {
	t.Helper()
	d, _ := newTest(t, act, action, false)
	a := &fakeAssistant{}
	d.opt.Assistant = a
	return d, a
}

func TestAssistantActionWakes(t *testing.T) {
	f := &fakeAct{}
	d, a := newAsst(t, f, "assistant")
	hit(d)
	time.Sleep(50 * time.Millisecond)
	if a.wakes != 1 || len(f.got()) != 0 {
		t.Fatalf("wakes %d, actuator %v", a.wakes, f.got())
	}
	if h := d.state().Hits[0]; !h.Fired || h.Debounced {
		t.Fatalf("hit %+v", h)
	}
	hit(d) // not debounced against itself
	if a.wakes != 2 {
		t.Fatalf("wakes %d", a.wakes)
	}
}

func TestAssistantWakesWhileBusy(t *testing.T) {
	d, a := newAsst(t, &fakeAct{}, "assistant")
	a.setBusy(true)
	hit(d)
	if a.wakes != 1 {
		t.Fatalf("barge-in: wakes %d", a.wakes)
	}
}

func TestAssistantHitMarkedFalse(t *testing.T) {
	d, a := newAsst(t, &fakeAct{}, "assistant")
	hit(d)
	a.noReq()
	d.waitIO()
	h := d.state().Hits[0]
	if h.Verdict != "false" || !h.Auto || h.FalseFile == "" {
		t.Fatalf("hit %+v", h)
	}
	if n, _ := filepath.Glob(filepath.Join(d.wordDir("x"), "false-*.wav")); len(n) == 0 {
		t.Fatal("false clip not saved")
	}
}

func TestAssistantAudioEveryBlock(t *testing.T) {
	d, a := newAsst(t, &fakeAct{}, "")
	d.onAudio(make([]int16, 320))
	d.onAudio(make([]int16, 320))
	if a.blocks != 2 {
		t.Fatalf("blocks %d", a.blocks)
	}
}

func TestAssistantDryRunOnlyLogs(t *testing.T) {
	d, a := newAsst(t, &fakeAct{}, "assistant")
	d.settings.DryRun = true
	hit(d)
	if a.wakes != 0 || len(d.state().Hits) != 1 {
		t.Fatalf("wakes %d", a.wakes)
	}
}

func TestQuickCommandsSuppressedWhileBusy(t *testing.T) {
	f := &fakeAct{}
	d, a := newAsst(t, f, "pause")
	a.setBusy(true)
	hit(d)
	time.Sleep(50 * time.Millisecond)
	h := d.state().Hits[0]
	if len(f.got()) != 0 || h.Fired || !h.Debounced {
		t.Fatalf("calls %v hit %+v", f.got(), h)
	}
}

func TestQuickCommandsWhileAssistantErrored(t *testing.T) {
	f := &fakeAct{}
	d, a := newAsst(t, f, "pause")
	a.setBusy(false)
	hit(d)
	deadline := time.Now().Add(2 * time.Second)
	for len(f.got()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if g := f.got(); len(g) != 1 || g[0] != "pause" {
		t.Fatalf("calls %v", g)
	}
}

func TestAssistantActionValid(t *testing.T) {
	d, _ := newAsst(t, &fakeAct{}, "")
	srv := httptest.NewServer(d.Handler())
	defer srv.Close()
	if r := post(t, srv.URL+"/api/words/x", `{"action":"assistant"}`); r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
}

func TestAssistantSettingsSaved(t *testing.T) {
	d, _ := newAsst(t, &fakeAct{}, "")
	srv := httptest.NewServer(d.Handler())
	defer srv.Close()
	r := post(t, srv.URL+"/api/settings",
		`{"assistant":{"model":"gemini-3.6-flash","tts":"gemini","voice":" Puck ","city":"  Санкт-Петербург "}}`)
	if r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	want := AssistantSettings{Model: "gemini-3.6-flash", TTS: "gemini", Voice: "Puck", City: "Санкт-Петербург"}
	if got := d.state().Assistant.Settings; got != want {
		t.Fatalf("state %+v", got)
	}
	if got := d.AssistantSettings(); got != want {
		t.Fatalf("AssistantSettings %+v", got)
	}
	b, err := os.ReadFile(d.settingsPath())
	if err != nil {
		t.Fatal(err)
	}
	var saved Settings
	if err := json.Unmarshal(b, &saved); err != nil || saved.Assistant != want {
		t.Fatalf("settings.json %+v %v", saved.Assistant, err)
	}
	// A partial update keeps the rest; "" is Piper and the judge's model.
	if r := post(t, srv.URL+"/api/settings", `{"assistant":{"tts":"","model":""}}`); r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	want = AssistantSettings{TTS: "piper", Voice: "Puck", City: "Санкт-Петербург"}
	if got := d.AssistantSettings(); got != want {
		t.Fatalf("after partial %+v", got)
	}
}

func TestAssistantSettingsDefaultPiper(t *testing.T) {
	d, _ := newAsst(t, &fakeAct{}, "")
	if got := d.AssistantSettings(); got.TTS != "piper" {
		t.Fatalf("default %+v", got)
	}
}

func TestAssistantSettingsRejectBadModel(t *testing.T) {
	d, _ := newAsst(t, &fakeAct{}, "")
	srv := httptest.NewServer(d.Handler())
	defer srv.Close()
	for _, body := range []string{
		`{"assistant":{"model":"Gemini Pro!"}}`,
		`{"assistant":{"tts":"espeak"}}`,
		`{"assistant":{"voice":"` + strings.Repeat("я", 65) + `"}}`,
		`{"assistant":{"city":"` + strings.Repeat("я", 65) + `"}}`,
		`{"assistant":{"model":"gemini-3.6-flash","tts":"espeak"}}`,
	} {
		if r := post(t, srv.URL+"/api/settings", body); r.StatusCode != 400 {
			t.Fatalf("%s: status %d", body, r.StatusCode)
		}
	}
	if got := d.AssistantSettings(); got != (AssistantSettings{TTS: "piper"}) {
		t.Fatalf("changed: %+v", got)
	}
	if r := post(t, srv.URL+"/api/settings", `{"assistant":{"city":"`+strings.Repeat("я", 64)+`"}}`); r.StatusCode != 200 {
		t.Fatalf("64 runes: status %d", r.StatusCode)
	}
}

func TestStateHidesSecrets(t *testing.T) {
	const key, token = "AIzaFAKE-gemini-key-123", "FAKE-refresh-token-456"
	d, _ := newAsst(t, &fakeAct{}, "")
	tokFile := filepath.Join(t.TempDir(), "spotify.json")
	os.WriteFile(tokFile, []byte(`{"client_id":"cid","refresh_token":"`+token+`"}`), 0o600)
	sp := spotifyapi.New(tokFile, "")
	d.opt.Judge, d.opt.JudgeModel = judge.New(key), judge.DefaultModel
	d.opt.SpotifyLinked = sp.Linked
	d.opt.AssistantStatus = func() any {
		return assistant.Status{State: "speaking", LastReply: "Сейчас 12:00", At: time.Unix(0, 0)}
	}
	srv := httptest.NewServer(d.Handler())
	defer srv.Close()
	r, err := http.Get(srv.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	r.Body.Close()
	s := string(b)
	if strings.Contains(s, key) || strings.Contains(s, token) {
		t.Fatalf("secret in state: %s", s)
	}
	for _, want := range []string{`"spotify_linked":true`, `"state":"speaking"`, `"last_reply":"Сейчас 12:00"`, `"available":true`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s in %s", want, s)
		}
	}
}

func TestStateWithoutAssistant(t *testing.T) {
	d, _ := newTest(t, &fakeAct{}, "", false)
	a := d.state().Assistant
	if a.Available || a.Status != nil || a.SpotifyLinked {
		t.Fatalf("view %+v", a)
	}
}
