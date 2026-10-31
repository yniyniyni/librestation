package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"voice/internal/led/proto"
	"voice/internal/led/scene"
)

type fakeScene struct {
	settings scene.Settings
	ai       []bool
	aiState  []string
}

func (f *fakeScene) SetSettings(s scene.Settings) { f.settings = s }
func (f *fakeScene) AI(on bool, state string) {
	f.ai = append(f.ai, on)
	f.aiState = append(f.aiState, state)
}

type fakeTimer struct {
	d       time.Duration
	f       func()
	stopped bool
}

func (t *fakeTimer) Stop() bool { t.stopped = true; return true }

func newTestAPI(t *testing.T) (*api, *fakeScene, *[]*fakeTimer) {
	t.Helper()
	fs := &fakeScene{}
	var timers []*fakeTimer
	a := newAPI(t.TempDir(), fs, func(d time.Duration, f func()) stopper {
		tm := &fakeTimer{d: d, f: f}
		timers = append(timers, tm)
		return tm
	})
	return a, fs, &timers
}

func post(a *api, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest("POST", path, strings.NewReader(body)))
	return w
}

func TestSettingsDefaultsWhenMissing(t *testing.T) {
	dir := t.TempDir()
	if got := loadSettings(dir); got != scene.DefaultSettings() {
		t.Fatalf("missing file: got %+v", got)
	}
	os.WriteFile(filepath.Join(dir, "led.json"), []byte(`{"volume_screen":"bogus"}`), 0o644)
	if got := loadSettings(dir); got != scene.DefaultSettings() {
		t.Fatalf("invalid file: got %+v", got)
	}
	os.WriteFile(filepath.Join(dir, "led.json"), []byte(`not json`), 0o644)
	if got := loadSettings(dir); got != scene.DefaultSettings() {
		t.Fatalf("garbage file: got %+v", got)
	}
}

func TestSettingsSaveAndReload(t *testing.T) {
	a, fs, _ := newTestAPI(t)
	want := scene.DefaultSettings()
	want.Flash = "white"
	body := `{"volume_screen":"` + want.VolumeScreen + `","ai_ring":"` + want.AIRing + `","ai_screen":"` + want.AIScreen + `","flash":"white","music":"` + want.Music + `"}`
	w := post(a, "/api/led/settings", body)
	if w.Code != 200 {
		t.Fatalf("code %d: %s", w.Code, w.Body)
	}
	if fs.settings != want {
		t.Fatalf("scene got %+v", fs.settings)
	}
	if got := loadSettings(a.dir); got != want {
		t.Fatalf("reload got %+v", got)
	}
	if !strings.Contains(w.Body.String(), `"flash":"white"`) || !strings.Contains(w.Body.String(), `"ai":false`) {
		t.Fatalf("body %s", w.Body)
	}
	g := httptest.NewRecorder()
	a.Handler().ServeHTTP(g, httptest.NewRequest("GET", "/api/led", nil))
	if g.Code != 200 || !strings.Contains(g.Body.String(), `"flash":"white"`) {
		t.Fatalf("GET: %d %s", g.Code, g.Body)
	}
	if ents, _ := os.ReadDir(a.dir); len(ents) != 1 {
		t.Fatalf("stray files in data dir: %v", ents)
	}
}

func TestPostSettingsRejectsUnknown(t *testing.T) {
	a, fs, _ := newTestAPI(t)
	if w := post(a, "/api/led/settings", `{"flash":"white","volume_screen":"pulse","ai_ring":"carousel","ai_screen":"dots","music":"spectrum"}`); w.Code != 200 {
		t.Fatalf("setup: %d %s", w.Code, w.Body)
	}
	before, _ := os.ReadFile(filepath.Join(a.dir, "led.json"))
	for _, body := range []string{
		`{"volume_screen":"pulse","ai_ring":"carousel","ai_screen":"dots","flash":"disco","music":"spectrum"}`,
		`{"volume_screen":"pulse","ai_ring":"carousel","ai_screen":"dots","flash":"sweep","music":"spectrum","extra":1}`,
		`nope`,
	} {
		if w := post(a, "/api/led/settings", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: code %d", body, w.Code)
		}
	}
	after, _ := os.ReadFile(filepath.Join(a.dir, "led.json"))
	if string(before) != string(after) {
		t.Fatalf("file changed: %s -> %s", before, after)
	}
	if fs.settings.Flash != "white" {
		t.Fatalf("scene settings changed: %+v", fs.settings)
	}
}

func TestAIDemoTurnsOffAfter30s(t *testing.T) {
	a, fs, timers := newTestAPI(t)
	if w := post(a, "/api/led/ai", `{"on":true}`); w.Code != 200 {
		t.Fatalf("code %d", w.Code)
	}
	if len(*timers) != 1 || (*timers)[0].d != 30*time.Second {
		t.Fatalf("timers %+v", *timers)
	}
	post(a, "/api/led/ai", `{"on":true}`) // newer POST resets the timer
	if !(*timers)[0].stopped || len(*timers) != 2 {
		t.Fatalf("timer not reset: %+v", *timers)
	}
	(*timers)[0].f() // stale timer must not turn off
	if last := fs.ai[len(fs.ai)-1]; !last {
		t.Fatal("stale timer turned AI off")
	}
	(*timers)[1].f()
	if last := fs.ai[len(fs.ai)-1]; last {
		t.Fatal("AI still on after 30 s")
	}
	post(a, "/api/led/ai", `{"on":true}`)
	post(a, "/api/led/ai", `{"on":false}`)
	if fs.ai[len(fs.ai)-1] || !(*timers)[2].stopped {
		t.Fatal("on:false must turn off now and cancel timer")
	}
}

func TestSocketAITimesOut(t *testing.T) {
	a, fs, timers := newTestAPI(t)
	handleEvent(nil, a, proto.Event{T: "ai", On: true, A: "thinking"})
	if len(*timers) != 1 || (*timers)[0].d != aiSafetyTimeout || !fs.ai[0] || fs.aiState[0] != "thinking" {
		t.Fatalf("timers %v ai %v", len(*timers), fs.ai)
	}
	handleEvent(nil, a, proto.Event{T: "ai", On: true, A: "error"})
	if len(*timers) != 2 || (*timers)[1].d != scene.AIErrorHold || !(*timers)[0].stopped {
		t.Fatalf("error: timers %d", len(*timers))
	}
	(*timers)[1].f()
	if fs.ai[len(fs.ai)-1] || a.ai {
		t.Fatal("error state did not expire")
	}
	handleEvent(nil, a, proto.Event{T: "ai"})
	if len(*timers) != 2 || fs.ai[len(fs.ai)-1] {
		t.Fatalf("off: timers %d ai %v", len(*timers), fs.ai)
	}
}

func TestBandsPadded(t *testing.T) {
	got := bands12([]float64{1, 2, 3})
	if got[2] != 3 || got[3] != 0 {
		t.Fatalf("%v", got)
	}
	long := make([]float64, 20)
	long[11], long[12] = 5, 9
	if g := bands12(long); g[11] != 5 {
		t.Fatalf("%v", g)
	}
}
