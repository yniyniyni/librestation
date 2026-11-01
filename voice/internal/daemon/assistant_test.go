package daemon

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"voice/internal/assistant"
	miccap "voice/internal/capture"
	"voice/internal/judge"
	"voice/internal/spotifyapi"
	"voice/internal/ycmd"
)

type fakeAssistant struct {
	mu     sync.Mutex
	busy   bool
	wakes  int
	blocks int
	live   [][]int16 // LiveAudio blocks
	hushes int
	state  string // "" = from busy
	spoke  time.Time
	noReq  func()
}

func (f *fakeAssistant) SpokeAt() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.spoke }

func (f *fakeAssistant) LiveAudio(p []int16) {
	f.mu.Lock()
	f.live = append(f.live, append([]int16(nil), p...))
	f.mu.Unlock()
}
func (f *fakeAssistant) Audio([]int16) { f.mu.Lock(); f.blocks++; f.mu.Unlock() }
func (f *fakeAssistant) Wake(_ context.Context, n func()) {
	f.mu.Lock()
	f.wakes++
	f.noReq = n
	f.mu.Unlock()
}
func (f *fakeAssistant) Busy() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.busy }
func (f *fakeAssistant) Hush()      { f.mu.Lock(); f.hushes++; f.mu.Unlock() }
func (f *fakeAssistant) State() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state != "" {
		return f.state
	}
	if f.busy {
		return "speaking"
	}
	return "idle"
}
func (f *fakeAssistant) setState(s string) {
	f.mu.Lock()
	f.state = s
	f.mu.Unlock()
}
func (f *fakeAssistant) hushed() int { f.mu.Lock(); defer f.mu.Unlock(); return f.hushes }
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

func TestAssistantSuppressedWhileSpeaking(t *testing.T) {
	d, a := newAsst(t, &fakeAct{}, "assistant")
	a.setBusy(true) // State() returns "speaking"
	hit(d)
	if a.wakes != 0 {
		t.Fatalf("speaking: wakes %d, want 0 (echo suppression)", a.wakes)
	}
	if h := d.state().Hits[0]; h.Fired {
		t.Fatalf("hit should not fire during speaking: %+v", h)
	}
}

func TestAssistantWakesDuringFollowup(t *testing.T) {
	d, a := newAsst(t, &fakeAct{}, "assistant")
	a.setState("followup")
	hit(d)
	if a.wakes != 1 {
		t.Fatalf("followup: wakes %d, want 1", a.wakes)
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

// TestAssistantLiveAudio: channel 0 of a raw block reaches the assistant as
// one 16 kHz block of 320 samples at x1 times LiveGain, with or without the
// Yandex model; without an assistant nothing is computed.
func TestAssistantLiveAudio(t *testing.T) {
	blk := make([]int16, miccap.BlockFrames*miccap.Channels)
	for i := 0; i < miccap.BlockFrames; i++ {
		blk[i*miccap.Channels] = int16(8000 * math.Sin(2*math.Pi*1000*float64(i)/48000))
		blk[i*miccap.Channels+1] = 30000 // other channels are not used
	}
	d, a := newAsst(t, &fakeAct{}, "")
	for i := 0; i < 30; i++ { // let the DC-removing mixer and decimator settle
		d.spotCommands(blk)
	}
	if len(a.live) != 30 || len(a.live[29]) != 320 {
		t.Fatalf("blocks %d", len(a.live))
	}
	var peak int16
	for _, v := range a.live[29] {
		peak = max(peak, v)
	}
	// 8000 at 1 kHz passes the decimator about unchanged: x2 -> ~16000.
	if peak < 13000 || peak > 18000 {
		t.Fatalf("peak %d, want about 16000", peak)
	}
	if d.yin == nil || d.spot != nil {
		t.Fatalf("yin %v spot %v", d.yin, d.spot)
	}

	d2, err := New(Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	d2.spotCommands(blk)
	if d2.yin != nil {
		t.Fatal("channel 0 computed without assistant and model")
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
	// A partial update keeps the rest; "" is Gemini and the judge's model.
	if r := post(t, srv.URL+"/api/settings", `{"assistant":{"tts":"","model":""}}`); r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	want = AssistantSettings{TTS: "gemini", Voice: "Puck", City: "Санкт-Петербург"}
	if got := d.AssistantSettings(); got != want {
		t.Fatalf("after partial %+v", got)
	}
}

func TestAssistantSettingsDefaultGemini(t *testing.T) {
	d, _ := newAsst(t, &fakeAct{}, "")
	if got := d.AssistantSettings(); got.TTS != "gemini" {
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
	if got := d.AssistantSettings(); got != (AssistantSettings{TTS: "gemini"}) {
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
	d.opt.SpotifyLinked = func() (bool, bool) { return sp.Linked(), true }
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
	if a.Available || a.Status != nil || a.SpotifyLinked != nil {
		t.Fatalf("view %+v", a)
	}
}

func TestAssistantDefaultModelFollowsJudge(t *testing.T) {
	d, _ := newAsst(t, &fakeAct{}, "")
	d.opt.JudgeModel = judge.DefaultModel
	if m := d.AssistantModel(); m != judge.DefaultModel {
		t.Fatalf("model %q", m)
	}
	srv := httptest.NewServer(d.Handler())
	defer srv.Close()
	if r := post(t, srv.URL+"/api/settings", `{"judge_model":"gemini-3.8-flash"}`); r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	if v := d.state().Assistant.DefaultModel; v != "gemini-3.8-flash" {
		t.Fatalf("default_model %q", v)
	}
	if m := d.AssistantModel(); m != "gemini-3.8-flash" {
		t.Fatalf("resolved %q", m)
	}
	if r := post(t, srv.URL+"/api/settings", `{"assistant":{"model":"gemini-3.6-flash"}}`); r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	if m := d.AssistantModel(); m != "gemini-3.6-flash" {
		t.Fatalf("own model %q", m)
	}
}

func TestSpotifyLinkUnknownIsNull(t *testing.T) {
	d, _ := newAsst(t, &fakeAct{}, "")
	known := false
	d.opt.SpotifyLinked = func() (bool, bool) { return false, known }
	if v := d.state().Assistant.SpotifyLinked; v != nil {
		t.Fatalf("unknown: %v", *v)
	}
	known = true
	if v := d.state().Assistant.SpotifyLinked; v == nil || *v {
		t.Fatalf("known: %v", v)
	}
}

func TestPiperMissingShown(t *testing.T) {
	d, _ := newAsst(t, &fakeAct{}, "")
	if d.state().Assistant.PiperMissing {
		t.Fatal("missing without a callback")
	}
	d.opt.PiperMissing = func() bool { return true }
	if !d.state().Assistant.PiperMissing {
		t.Fatal("not shown")
	}
}

func TestQuickViewWithoutModel(t *testing.T) {
	d, _ := newTest(t, &fakeAct{}, "", false)
	d.opt.CommandsErr = "нет модели"
	q := d.state().Quick
	if q.Available || q.Error != "нет модели" || q.Off {
		t.Fatalf("quick %+v", q)
	}
	var ids []string
	for _, g := range q.Groups {
		if !g.On || g.Words == "" {
			t.Fatalf("group %+v", g)
		}
		ids = append(ids, g.ID)
	}
	if strings.Join(ids, ",") != "pause,next,prev,volume,play" {
		t.Fatalf("groups %v", ids)
	}
	if q.Groups[0].Words != "стоп, хватит, пауза" {
		t.Fatalf("words %q", q.Groups[0].Words)
	}
}

func TestQuickGroupsCoverCommands(t *testing.T) {
	seen := map[string]bool{}
	for _, g := range quickGroups {
		for _, w := range strings.Split(g.words, ", ") {
			if quickCommands[w].group != g.id {
				t.Errorf("%q listed under %s, command table says %q", w, g.id, quickCommands[w].group)
			}
			seen[w] = true
		}
	}
	for w := range quickCommands {
		if !seen[w] {
			t.Errorf("%q is in no group", w)
		}
	}
}

func TestQuickSettingsSaved(t *testing.T) {
	d, _ := newTest(t, &fakeAct{}, "", false)
	d.opt.Commands = &ycmd.Model{}
	srv := httptest.NewServer(d.Handler())
	defer srv.Close()
	for _, body := range []string{`{"quick":{"group":"next","on":false}}`, `{"quick":{"off":true}}`} {
		if r := post(t, srv.URL+"/api/settings", body); r.StatusCode != 200 {
			t.Fatalf("%s: status %d", body, r.StatusCode)
		}
	}
	q := d.state().Quick
	if !q.Available || !q.Off {
		t.Fatalf("quick %+v", q)
	}
	for _, g := range q.Groups {
		if g.On != (g.ID != "next") {
			t.Fatalf("group %+v", g)
		}
	}
	b, err := os.ReadFile(d.settingsPath())
	var saved Settings
	if err != nil || json.Unmarshal(b, &saved) != nil || !saved.Quick.Off || !saved.Quick.Disabled["next"] {
		t.Fatalf("settings.json %+v %v", saved.Quick, err)
	}
	if r := post(t, srv.URL+"/api/settings", `{"quick":{"group":"next","on":true}}`); r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	if d.state().Quick.Groups[1].On != true || len(d.settings.Quick.Disabled) != 0 {
		t.Fatalf("disabled %v", d.settings.Quick.Disabled)
	}
}

func TestQuickSettingsUnknownGroup(t *testing.T) {
	d, _ := newTest(t, &fakeAct{}, "", false)
	srv := httptest.NewServer(d.Handler())
	defer srv.Close()
	for _, body := range []string{`{"quick":{"group":"nope","on":false}}`, `{"quick":{"on":false}}`} {
		if r := post(t, srv.URL+"/api/settings", body); r.StatusCode != 400 {
			t.Fatalf("%s: status %d", body, r.StatusCode)
		}
	}
	if len(d.settings.Quick.Disabled) != 0 {
		t.Fatalf("disabled %v", d.settings.Quick.Disabled)
	}
}
