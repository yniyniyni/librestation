package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"voice/internal/led/scene"
)

const aiDemoTimeout = 30 * time.Second

// sceneSink is the part of *scene.Scene the HTTP API drives.
type sceneSink interface {
	SetSettings(scene.Settings)
	AI(bool)
}

type stopper interface{ Stop() bool }

// afterFunc schedules f after d; injected so tests need not sleep.
type afterFunc func(d time.Duration, f func()) stopper

func realAfter(d time.Duration, f func()) stopper { return time.AfterFunc(d, f) }

// loadSettings reads <dir>/led.json; a missing or invalid file gives defaults.
func loadSettings(dir string) scene.Settings {
	def := scene.DefaultSettings()
	b, err := os.ReadFile(filepath.Join(dir, "led.json"))
	if err != nil {
		return def
	}
	s, err := parseSettings(b)
	if err != nil {
		return def
	}
	return s
}

func parseSettings(b []byte) (scene.Settings, error) {
	var s scene.Settings
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return s, err
	}
	return s, s.Validate()
}

// saveSettings writes <dir>/led.json atomically (tmp + rename).
func saveSettings(dir string, s scene.Settings) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "led.json.tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // no-op after a successful rename
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, "led.json"))
}

// api owns the settings and AI-mode state behind the HTTP endpoints.
type api struct {
	dir   string
	scene sceneSink
	after afterFunc

	mu       sync.Mutex
	settings scene.Settings
	ai       bool
	gen      int // bumped on every AI change; stale timers do nothing
	timer    stopper
}

func newAPI(dir string, sc sceneSink, after afterFunc) *api {
	a := &api{dir: dir, scene: sc, after: after, settings: loadSettings(dir)}
	sc.SetSettings(a.settings)
	return a
}

// setAI switches AI mode; timeout > 0 turns it off again after that long.
func (a *api) setAI(on bool, timeout time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
	a.gen++
	a.ai = on
	a.scene.AI(on)
	if on && timeout > 0 {
		gen := a.gen
		a.timer = a.after(timeout, func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.gen != gen {
				return
			}
			a.timer = nil
			a.ai = false
			a.scene.AI(false)
		})
	}
}

func (a *api) writeState(w http.ResponseWriter) {
	a.mu.Lock()
	b, err := json.Marshal(struct {
		Settings scene.Settings `json:"settings"`
		AI       bool           `json:"ai"`
	}{a.settings, a.ai})
	a.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

func httpError(w http.ResponseWriter, code int, err error) {
	http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), code)
}

// Handler serves /api/led, /api/led/settings and /api/led/ai.
func (a *api) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/led", func(w http.ResponseWriter, r *http.Request) { a.writeState(w) })
	mux.HandleFunc("POST /api/led/settings", func(w http.ResponseWriter, r *http.Request) {
		var body bytes.Buffer
		if _, err := body.ReadFrom(http.MaxBytesReader(w, r.Body, 4096)); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		s, err := parseSettings(body.Bytes())
		if err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		a.mu.Lock()
		if err := saveSettings(a.dir, s); err != nil {
			a.mu.Unlock()
			httpError(w, http.StatusInternalServerError, err)
			return
		}
		a.settings = s
		a.scene.SetSettings(s)
		a.mu.Unlock()
		a.writeState(w)
	})
	mux.HandleFunc("POST /api/led/ai", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			On bool `json:"on"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		a.setAI(req.On, aiDemoTimeout)
		a.writeState(w)
	})
	return mux
}

// bands12 takes the first 12 values of b, zero-padded.
func bands12(b []float64) (out [12]float64) {
	copy(out[:], b)
	return out
}
