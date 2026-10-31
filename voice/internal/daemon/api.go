package daemon

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"voice/internal/kws"
	"voice/internal/wav"
)

//go:embed ui/index.html
var indexHTML []byte

type wordView struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Enabled     bool      `json:"enabled"`
	Sensitivity float32   `json:"sensitivity"`
	Action      string    `json:"action"`
	Threshold   float32   `json:"threshold"`
	Recall      float64   `json:"recall"`
	FalsePerH   float64   `json:"false_per_h"`
	Takes       int       `json:"takes"`
	FalseClips  int       `json:"false_clips"`
	NewFalses   int       `json:"new_falses"` // since the last training; autoRetrainFalses retrains
	Created     time.Time `json:"created"`
	Score       float32   `json:"score"`
	Notes       []string  `json:"notes,omitempty"`
}

type enrollView struct {
	Name       string    `json:"name"`
	Takes      []float64 `json:"takes"` // detected word length per take
	HasAmbient bool      `json:"has_ambient"`
	SpeechSecs float64   `json:"speech_secs"`
	MinTakes   int       `json:"min_takes"`
}

type stateView struct {
	Station string `json:"station"`
	Audio   struct {
		OK    bool    `json:"ok"`
		Error string  `json:"error,omitempty"`
		Level float64 `json:"level"`
	} `json:"audio"`
	Words    []wordView   `json:"words"`
	Hits     []Hit        `json:"hits"`
	Enroll   *enrollView  `json:"enroll"`
	Train    trainState   `json:"train"`
	Judge    judgeView    `json:"judge"`
	Settings settingsView `json:"settings"`
}

// settingsView is the part of Settings the hits card edits.
type settingsView struct {
	Judge         bool `json:"judge"`
	AutoFalseUndo bool `json:"auto_false_undo"`
}

func (d *Daemon) state() stateView {
	d.mu.Lock()
	defer d.mu.Unlock()
	var v stateView
	v.Station = d.stationName()
	v.Audio.OK, v.Audio.Error, v.Audio.Level = d.audioOK, d.audioErr, d.level
	v.Words = make([]wordView, 0, len(d.words)) // [] not null for the UI
	scores := map[string]float32{}
	if d.det != nil {
		for i, id := range d.detWords {
			scores[id] = d.det.Scores[i]
		}
	}
	for _, w := range d.words {
		s := d.settings.Words[w.ID]
		falses, _ := filepath.Glob(filepath.Join(d.wordDir(w.ID), "false-*.wav"))
		v.Words = append(v.Words, wordView{
			ID: w.ID, Name: w.Model.Name, Enabled: s != nil && s.Enabled, Sensitivity: s.Sensitivity, Action: s.Action,
			Threshold: w.Model.Threshold, Recall: w.Model.Recall, FalsePerH: w.Model.FalsePerH,
			Takes: w.Model.Takes, FalseClips: len(falses), NewFalses: d.newFalses(w.ID), Created: w.Model.Created,
			Score: scores[w.ID], Notes: w.Model.TrainNotes,
		})
	}
	v.Hits = make([]Hit, len(d.hits))
	for i, h := range d.hits {
		v.Hits[len(d.hits)-1-i] = *h // newest first; copied, it changes after unlock
	}
	v.Settings = settingsView{Judge: !d.settings.JudgeOff, AutoFalseUndo: d.settings.AutoFalseUndo}
	v.Judge = d.judgeViewLocked()
	if e := d.enroll; e != nil {
		ev := &enrollView{Name: e.Name, HasAmbient: len(e.Ambient) > 0, MinTakes: minTakes,
			SpeechSecs: float64(len(e.Speech)) / kws.SampleRate, Takes: []float64{}}
		for _, t := range e.Takes {
			s, end, _ := kws.FindSpeech(t)
			ev.Takes = append(ev.Takes, float64(end-s)/kws.SampleRate)
		}
		v.Enroll = ev
	}
	v.Train = d.train
	return v
}

// Handler serves the UI and the JSON API.
func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, d.state())
	})
	mux.HandleFunc("POST /api/words/{id}", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Enabled     *bool    `json:"enabled"`
			Sensitivity *float32 `json:"sensitivity"`
			Action      *string  `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpErr(w, err)
			return
		}
		action := ""
		if req.Action != nil {
			a, err := normalizeAction(*req.Action)
			if err != nil {
				httpErr(w, err)
				return
			}
			action = a
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		s := d.settings.Words[r.PathValue("id")]
		if s == nil {
			httpErr(w, errors.New("нет такого слова"))
			return
		}
		if req.Enabled != nil {
			s.Enabled = *req.Enabled
		}
		if req.Action != nil {
			s.Action = action
		}
		if req.Sensitivity != nil {
			s.Sensitivity = max(-0.3, min(0.3, *req.Sensitivity))
		}
		if err := d.saveSettings(); err != nil {
			httpErr(w, err)
			return
		}
		if err := d.rebuildDetector(); err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, s)
	})
	mux.HandleFunc("DELETE /api/words/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if wordID(id) != id {
			httpErr(w, errors.New("bad id"))
			return
		}
		d.mu.Lock()
		delete(d.settings.Words, id)
		d.saveSettings()
		d.mu.Unlock()
		if err := os.RemoveAll(d.wordDir(id)); err != nil {
			httpErr(w, err)
			return
		}
		if err := d.loadWords(); err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/words/{id}/retrain", func(w http.ResponseWriter, r *http.Request) {
		if err := d.startTraining(r.PathValue("id"), nil); err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/hits/{n}", func(w http.ResponseWriter, r *http.Request) {
		h := d.hit(r.PathValue("n"))
		if h == nil {
			http.NotFound(w, r)
			return
		}
		a, err := d.hitAudio(h)
		if err != nil {
			httpErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(wav.Encode(a, kws.SampleRate))
	})
	mux.HandleFunc("POST /api/hits/{n}/judge", func(w http.ResponseWriter, r *http.Request) {
		h := d.hit(r.PathValue("n"))
		if h == nil {
			http.NotFound(w, r)
			return
		}
		if !d.rejudge(h) {
			httpErr(w, errors.New("модель не настроена: нет ключа"))
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	// {verdict} is "false" (keep the clip as a hard negative of the word) or
	// "ok" (the hit was right; takes back a clip saved for it).
	mux.HandleFunc("POST /api/hits/{n}/{verdict}", func(w http.ResponseWriter, r *http.Request) {
		h := d.hit(r.PathValue("n"))
		if h == nil {
			http.NotFound(w, r)
			return
		}
		if err := d.setVerdict(h, r.PathValue("verdict")); err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/settings", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Judge         *bool `json:"judge"`
			AutoFalseUndo *bool `json:"auto_false_undo"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpErr(w, err)
			return
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if req.Judge != nil {
			d.settings.JudgeOff = !*req.Judge
		}
		if req.AutoFalseUndo != nil {
			d.settings.AutoFalseUndo = *req.AutoFalseUndo
		}
		if err := d.saveSettings(); err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/record", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Seconds int `json:"seconds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpErr(w, err)
			return
		}
		if req.Seconds < 1 || req.Seconds > maxRecordSecs {
			http.Error(w, fmt.Sprintf(`{"error":"seconds must be 1..%d"}`, maxRecordSecs), http.StatusBadRequest)
			return
		}
		if err := d.startRecording(req.Seconds); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusConflict)
			return
		}
		writeJSON(w, d.recordStatus())
	})
	mux.HandleFunc("GET /api/record", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, d.recordStatus())
	})
	mux.HandleFunc("POST /api/enroll/start", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpErr(w, err)
			return
		}
		if _, err := d.startEnroll(req.Name); err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, d.state().Enroll)
	})
	mux.HandleFunc("POST /api/enroll/cancel", func(w http.ResponseWriter, r *http.Request) {
		d.endEnroll()
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/enroll/ambient", func(w http.ResponseWriter, r *http.Request) {
		level, err := d.recordAmbient(r.Context())
		if err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, map[string]float64{"level": level})
	})
	mux.HandleFunc("POST /api/enroll/take", func(w http.ResponseWriter, r *http.Request) {
		res, err := d.recordTake(r.Context())
		if err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, res)
	})
	mux.HandleFunc("DELETE /api/enroll/take/{i}", func(w http.ResponseWriter, r *http.Request) {
		i, _ := strconv.Atoi(r.PathValue("i"))
		if err := d.deleteTake(i); err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, d.state().Enroll)
	})
	// {i} is a take index or "last" (the latest take even if it was rejected).
	mux.HandleFunc("GET /api/enroll/take/{i}", func(w http.ResponseWriter, r *http.Request) {
		i, err := strconv.Atoi(r.PathValue("i"))
		d.mu.Lock()
		var pcm []int16
		switch {
		case d.enroll == nil:
		case r.PathValue("i") == "last":
			pcm = d.enroll.Last
		case err == nil && i >= 0 && i < len(d.enroll.Takes):
			pcm = d.enroll.Takes[i]
		}
		d.mu.Unlock()
		if pcm == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(wav.Encode(pcm, kws.SampleRate))
	})
	mux.HandleFunc("POST /api/enroll/speech", func(w http.ResponseWriter, r *http.Request) {
		secs, _ := strconv.ParseFloat(r.URL.Query().Get("secs"), 64)
		level, err := d.recordSpeech(r.Context(), secs)
		if err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, map[string]float64{"level": level})
	})
	mux.HandleFunc("POST /api/enroll/train", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Action *string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			httpErr(w, err)
			return
		}
		if err := d.finishEnroll(req.Action); err != nil {
			httpErr(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	return mux
}

func (d *Daemon) hit(n string) *Hit {
	id, _ := strconv.Atoi(n)
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, h := range d.hits {
		if h.ID == id {
			return h
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpErr(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	if errors.Is(err, context.Canceled) {
		code = 499
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
