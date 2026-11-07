package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"voice/internal/judge"
	"voice/internal/kws"
	"voice/internal/wav"
)

// APIVersion is the version of the contract in docs/api.md.
const APIVersion = 1

// apiBase is the prefix of every API path.
const apiBase = "/api/v1"

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
	Alarms  *alarmsView `json:"alarms"` // null without alarms
	Station string      `json:"station"`
	Audio   struct {
		OK    bool    `json:"ok"`
		Error string  `json:"error,omitempty"`
		Level float64 `json:"level"`
	} `json:"audio"`
	Player struct {
		State string `json:"state"` // Soloist's status; "" until it has said
	} `json:"player"`
	Volume    *volumeView   `json:"volume"` // null without a volume control
	Words     []wordView    `json:"words"`
	Hits      []Hit         `json:"-"` // GET /hits and the "hits" event
	Enroll    *enrollView   `json:"enroll"`
	Train     trainState    `json:"train"`
	Judge     judgeView     `json:"judge"`
	Settings  settingsView  `json:"settings"`
	Assistant assistantView `json:"assistant"`
	Quick     quickView     `json:"quick"`
	StockWake stockWakeView `json:"stock_wake"`
}

// stockWakeView is the stock trigger words' card.
type stockWakeView struct {
	Error string          `json:"error,omitempty"`
	Words []stockWakeWord `json:"words"`
}

type stockWakeWord struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	On   bool   `json:"on"`
}

// volumeView is the one volume of Spotify, ring and voice.
type volumeView struct {
	Volume   int `json:"volume"`
	KnobStep int `json:"knob_step"`
}

// liveView is the fast part of the state: the "live" event.
type liveView struct {
	Level     float64            `json:"level"`
	AudioOK   bool               `json:"audio_ok"`
	Scores    map[string]float32 `json:"scores"`
	Assistant string             `json:"assistant,omitempty"`
	Volume    *int               `json:"volume,omitempty"`
}

// quickView is the quick-commands card.
type quickView struct {
	Available bool         `json:"available"` // Yandex's model is loaded
	Error     string       `json:"error,omitempty"`
	Off       bool         `json:"off"`
	Groups    []quickGroup `json:"groups"`
}

type quickGroup struct {
	ID    string `json:"id"`
	Words string `json:"words"`
	On    bool   `json:"on"`
}

// quickPatch is a partial update of QuickSettings from the panel: Off, and/or
// one group switched by Group and On.
type quickPatch struct {
	Off   *bool   `json:"off"`
	Group *string `json:"group"`
	On    *bool   `json:"on"`
}

func (p *quickPatch) apply(s *QuickSettings) error {
	if p.Off != nil {
		s.Off = *p.Off
	}
	if p.Group == nil && p.On == nil {
		return nil
	}
	if p.Group == nil || p.On == nil {
		return errors.New("для группы нужны group и on")
	}
	known := false
	for _, g := range quickGroups {
		known = known || g.id == *p.Group
	}
	if !known {
		return fmt.Errorf("неизвестная группа %q", *p.Group)
	}
	disabled := maps.Clone(s.Disabled)
	if *p.On {
		delete(disabled, *p.Group)
	} else {
		if disabled == nil {
			disabled = map[string]bool{}
		}
		disabled[*p.Group] = true
	}
	if len(disabled) == 0 {
		disabled = nil
	}
	s.Disabled = disabled
	return nil
}

// assistantView is the assistant card: no keys or tokens, only state.
type assistantView struct {
	Available     bool              `json:"available"` // the session is built (a Gemini key is present)
	Status        any               `json:"status"`
	SpotifyLinked *bool             `json:"spotify_linked"` // null until checked
	PiperMissing  bool              `json:"piper_missing"`  // Piper did not start: replies use Gemini's voice
	Settings      AssistantSettings `json:"settings"`
	LiveModel     string            `json:"live_model"`    // the model conversations run on
	DefaultModel  string            `json:"default_model"` // what Model "" means: the judge's current model
}

// EQPreset describes one equalizer preset for the UI.
type EQPreset struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// settingsView is the part of Settings the hits card edits.
type settingsView struct {
	Judge         bool       `json:"judge"`
	AutoFalseUndo bool       `json:"auto_false_undo"`
	ActivationOff bool       `json:"activation_off"`
	EQMode        int        `json:"eq_mode"`
	HasEQ         bool       `json:"has_eq"`
	EQPresets     []EQPreset `json:"eq_presets,omitempty"`
}

// volumeView reads the volume; never under d.mu, because a volume step holds
// the control's own lock while it talks to Soloist.
func (d *Daemon) volumeView() *volumeView {
	c := d.opt.Volume
	if c == nil {
		return nil
	}
	return &volumeView{Volume: c.Volume(), KnobStep: c.KnobStep()}
}

// state is a snapshot for the API. d.mu is the audio goroutine's lock too, so
// nothing here may touch the disk.
func (d *Daemon) state() stateView {
	var v stateView
	v.Volume = d.volumeView()
	v.Alarms = d.alarmsView()
	d.mu.Lock()
	defer d.mu.Unlock()
	v.Station = d.stationName()
	v.Audio.OK, v.Audio.Error, v.Audio.Level = d.audioOK, d.audioErr, d.level
	v.Player.State = d.playback
	v.Words = make([]wordView, 0, len(d.words)) // [] not null for the UI
	scores := map[string]float32{}
	if d.det != nil {
		for i, id := range d.detWords {
			scores[id] = d.det.Scores[i]
		}
	}
	for _, w := range d.words {
		s := d.settings.Words[w.ID]
		falses := d.falses[w.ID]
		v.Words = append(v.Words, wordView{
			ID: w.ID, Name: w.Model.Name, Enabled: s != nil && s.Enabled, Sensitivity: s.Sensitivity, Action: s.Action,
			Threshold: w.Model.Threshold, Recall: w.Model.Recall, FalsePerH: w.Model.FalsePerH,
			Takes: w.Model.Takes, FalseClips: falses.clips, NewFalses: falses.fresh, Created: w.Model.Created,
			Score: scores[w.ID], Notes: w.Model.TrainNotes,
		})
	}
	v.Hits = make([]Hit, len(d.hits))
	for i, h := range d.hits {
		v.Hits[len(d.hits)-1-i] = *h // newest first; copied, it changes after unlock
	}
	v.Settings = settingsView{Judge: !d.settings.JudgeOff, AutoFalseUndo: d.settings.AutoFalseUndo, ActivationOff: d.settings.ActivationOff,
		EQMode: d.settings.EQMode, HasEQ: d.opt.SetEQ != nil, EQPresets: d.opt.EQPresets}
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
	v.Assistant = d.assistantViewLocked()
	v.StockWake = stockWakeView{Error: d.opt.WakeErr, Words: make([]stockWakeWord, 0, len(d.opt.Wake))}
	for _, m := range d.opt.Wake {
		v.StockWake.Words = append(v.StockWake.Words, stockWakeWord{m.ID, m.Name, d.settings.StockWake[m.ID]})
	}
	v.Quick = quickView{Available: d.opt.Commands != nil, Error: d.opt.CommandsErr, Off: d.settings.Quick.Off,
		Groups: make([]quickGroup, 0, len(quickGroups))}
	for _, g := range quickGroups {
		v.Quick.Groups = append(v.Quick.Groups, quickGroup{ID: g.id, Words: g.words, On: !d.settings.Quick.Disabled[g.id]})
	}
	return v
}

// live is the fast part of the state, cheap enough for several times a second.
func (d *Daemon) live() liveView {
	var v liveView
	if c := d.opt.Volume; c != nil {
		n := c.Volume()
		v.Volume = &n
	}
	if a := d.opt.Assistant; a != nil {
		v.Assistant = a.State()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	v.Level, v.AudioOK = d.level, d.audioOK
	v.Scores = make(map[string]float32, len(d.detWords))
	if d.det != nil {
		for i, id := range d.detWords {
			v.Scores[id] = d.det.Scores[i]
		}
	}
	return v
}

// assistantViewLocked fills the assistant card; caller holds d.mu. The
// callbacks return at once (they read cached values).
func (d *Daemon) assistantViewLocked() assistantView {
	v := assistantView{Available: d.opt.Assistant != nil, Settings: d.settings.Assistant,
		LiveModel: d.opt.LiveModel, DefaultModel: d.assistantDefaultModelLocked()}
	if v.Settings.TTS == "" {
		v.Settings.TTS = "gemini"
	}
	if f := d.opt.AssistantStatus; f != nil {
		v.Status = f()
	}
	if f := d.opt.SpotifyLinked; f != nil {
		if linked, known := f(); known {
			v.SpotifyLinked = &linked
		}
	}
	if f := d.opt.PiperMissing; f != nil {
		v.PiperMissing = f()
	}
	return v
}

// assistantPatch is a partial update of AssistantSettings from the panel.
type assistantPatch struct {
	Model     *string `json:"model"`
	TTS       *string `json:"tts"`
	Voice     *string `json:"voice"`
	City      *string `json:"city"`
	EarconOff *bool   `json:"earcon_off"`
}

// apply validates the patch and applies it to s; s is unchanged on error.
func (p *assistantPatch) apply(s *AssistantSettings) error {
	n := *s
	if m := p.Model; m != nil {
		if *m != "" && !judge.ValidModel(*m) {
			return fmt.Errorf("неверное имя модели %q", *m)
		}
		n.Model = *m
	}
	if t := p.TTS; t != nil {
		switch *t {
		case "piper":
			n.TTS = "piper"
		case "", "gemini":
			n.TTS = "gemini"
		default:
			return fmt.Errorf("неизвестный голосовой движок %q", *t)
		}
	}
	for _, f := range []struct {
		in   *string
		out  *string
		what string
	}{{p.Voice, &n.Voice, "голос"}, {p.City, &n.City, "город"}} {
		if f.in == nil {
			continue
		}
		v := strings.TrimSpace(*f.in)
		if utf8.RuneCountInString(v) > maxAssistantText {
			return fmt.Errorf("%s длиннее %d символов", f.what, maxAssistantText)
		}
		*f.out = v
	}
	if p.EarconOff != nil {
		n.EarconOff = *p.EarconOff
	}
	*s = n
	return nil
}

// Handler serves the panel and the API of docs/api.md.
func (d *Daemon) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", serveIndex)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, notFound("нет такого адреса API"))
	})
	api := func(pattern string, f func(w http.ResponseWriter, r *http.Request) error) {
		method, path, _ := strings.Cut(pattern, " ")
		mux.HandleFunc(method+" "+apiBase+path, func(w http.ResponseWriter, r *http.Request) {
			if err := f(w, r); err != nil {
				writeErr(w, err)
			}
		})
	}
	if d.opt.LED != nil {
		mux.Handle(apiBase+"/led", d.opt.LED)
		mux.Handle(apiBase+"/led/", d.opt.LED)
	}
	api("GET /info", func(w http.ResponseWriter, r *http.Request) error {
		writeJSON(w, d.info())
		return nil
	})
	api("GET /state", func(w http.ResponseWriter, r *http.Request) error {
		writeJSON(w, d.state())
		return nil
	})
	mux.HandleFunc("GET "+apiBase+"/events", d.serveEvents)
	d.alarmRoutes(api)
	api("GET /hits", func(w http.ResponseWriter, r *http.Request) error {
		writeJSON(w, map[string][]Hit{"hits": d.state().Hits})
		return nil
	})
	api("GET /hits/{id}/audio", func(w http.ResponseWriter, r *http.Request) error {
		h, err := d.hitByPath(r)
		if err != nil {
			return err
		}
		a, err := d.hitAudio(h)
		if err != nil {
			return notFound("запись срабатывания не найдена")
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(wav.Encode(a, kws.SampleRate))
		return nil
	})
	// verdict "false" keeps the clip as a hard negative of the word, "ok"
	// says the hit was right and takes back a clip saved for it.
	api("PUT /hits/{id}/verdict", func(w http.ResponseWriter, r *http.Request) error {
		h, err := d.hitByPath(r)
		if err != nil {
			return err
		}
		var req struct {
			Verdict string `json:"verdict"`
		}
		if err := decode(r, &req); err != nil {
			return err
		}
		if err := d.setVerdict(h, req.Verdict); err != nil {
			return err
		}
		writeOK(w)
		return nil
	})
	api("POST /hits/{id}/judge", func(w http.ResponseWriter, r *http.Request) error {
		h, err := d.hitByPath(r)
		if err != nil {
			return err
		}
		if !d.rejudge(h) {
			return conflict("модель не настроена: нет ключа")
		}
		writeOK(w)
		return nil
	})
	api("PATCH /words/{id}", func(w http.ResponseWriter, r *http.Request) error {
		var req struct {
			Enabled     *bool    `json:"enabled"`
			Sensitivity *float32 `json:"sensitivity"`
			Action      *string  `json:"action"`
		}
		if err := decode(r, &req); err != nil {
			return err
		}
		action := ""
		if req.Action != nil {
			a, err := normalizeAction(*req.Action)
			if err != nil {
				return err
			}
			action = a
		}
		d.mu.Lock()
		s := d.settings.Words[r.PathValue("id")]
		if s == nil {
			d.mu.Unlock()
			return notFound("нет такого слова")
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
		err := d.rebuildDetector()
		view := *s
		d.mu.Unlock()
		if err != nil {
			return internal(err)
		}
		if err := d.persistSettings(); err != nil {
			return internal(err)
		}
		writeJSON(w, view)
		return nil
	})
	api("DELETE /words/{id}", func(w http.ResponseWriter, r *http.Request) error {
		id := r.PathValue("id")
		if wordID(id) != id {
			return notFound("нет такого слова")
		}
		if err := d.removeWord(id); err != nil {
			return internal(err)
		}
		writeOK(w)
		return nil
	})
	api("POST /words/{id}/retrain", func(w http.ResponseWriter, r *http.Request) error {
		if err := d.startTraining(r.PathValue("id"), nil); err != nil {
			return err
		}
		writeOK(w)
		return nil
	})
	api("PATCH /settings", func(w http.ResponseWriter, r *http.Request) error {
		var req struct {
			Judge         *bool           `json:"judge"`
			JudgeModel    *string         `json:"judge_model"`
			AutoFalseUndo *bool           `json:"auto_false_undo"`
			ActivationOff *bool           `json:"activation_off"`
			EQMode        *int            `json:"eq_mode"`
			Assistant     *assistantPatch `json:"assistant"`
			Quick         *quickPatch     `json:"quick"`
			StockWake     map[string]bool `json:"stock_wake"`
		}
		if err := decode(r, &req); err != nil {
			return err
		}
		if m := req.JudgeModel; m != nil && !judge.ValidModel(*m) {
			return badRequest(fmt.Sprintf("неверное имя модели %q", *m))
		}
		if m := req.EQMode; m != nil && (*m < 0 || *m >= len(d.opt.EQPresets)) {
			return badRequest(fmt.Sprintf("eq_mode %d вне диапазона 0-%d", *m, len(d.opt.EQPresets)-1))
		}
		for id := range req.StockWake {
			if !slices.ContainsFunc(d.opt.Wake, func(m WakeModel) bool { return m.ID == id }) {
				return badRequest(fmt.Sprintf("нет такого слова %q", id))
			}
		}
		d.mu.Lock()
		asst, quick := d.settings.Assistant, d.settings.Quick
		var err error
		if a := req.Assistant; a != nil {
			err = a.apply(&asst)
		}
		if q := req.Quick; q != nil && err == nil {
			err = q.apply(&quick)
		}
		if err != nil {
			d.mu.Unlock()
			return err
		}
		if req.Judge != nil {
			d.settings.JudgeOff = !*req.Judge
		}
		if m := req.JudgeModel; m != nil {
			d.settings.JudgeModel = *m
		}
		if req.AutoFalseUndo != nil {
			d.settings.AutoFalseUndo = *req.AutoFalseUndo
		}
		if req.ActivationOff != nil {
			d.settings.ActivationOff = *req.ActivationOff
		}
		var eqApply *int
		if m := req.EQMode; m != nil {
			d.settings.EQMode = *m
			eqApply = m
		}
		d.settings.Assistant, d.settings.Quick = asst, quick
		for id, on := range req.StockWake {
			if d.settings.StockWake == nil {
				d.settings.StockWake = map[string]bool{}
			}
			d.settings.StockWake[id] = on
		}
		d.mu.Unlock()
		if err := d.persistSettings(); err != nil {
			return internal(err)
		}
		if eqApply != nil && d.opt.SetEQ != nil {
			if err := d.opt.SetEQ(*eqApply); err != nil {
				log.Printf("eq: %v", err)
			}
		}
		writeOK(w)
		return nil
	})
	api("GET /volume", func(w http.ResponseWriter, r *http.Request) error {
		v := d.volumeView()
		if v == nil {
			return conflict("управления громкостью нет")
		}
		writeJSON(w, v)
		return nil
	})
	api("PATCH /volume", func(w http.ResponseWriter, r *http.Request) error {
		var req struct {
			Step     *int `json:"step"`
			KnobStep *int `json:"knob_step"`
		}
		if err := decode(r, &req); err != nil {
			return err
		}
		c := d.opt.Volume
		if c == nil {
			return conflict("управления громкостью нет")
		}
		if req.KnobStep != nil {
			if err := c.SetKnobStep(*req.KnobStep); err != nil {
				return badRequest(err.Error())
			}
		}
		if req.Step != nil {
			if *req.Step < -100 || *req.Step > 100 {
				return badRequest("step должен быть от -100 до 100")
			}
			if err := c.Step(*req.Step); err != nil {
				return conflict(err.Error())
			}
		}
		writeJSON(w, d.volumeView())
		return nil
	})
	api("POST /player/{action}", func(w http.ResponseWriter, r *http.Request) error {
		act := d.opt.Actuator
		if act == nil {
			return conflict("управления музыкой нет")
		}
		do := map[string]func() error{"pause": act.Pause, "play": act.Play, "next": act.Next, "prev": act.Prev}[r.PathValue("action")]
		if do == nil {
			return notFound("нет такой команды")
		}
		if err := do(); err != nil {
			return conflict(err.Error())
		}
		writeOK(w)
		return nil
	})
	api("GET /record", func(w http.ResponseWriter, r *http.Request) error {
		writeJSON(w, d.recordStatus())
		return nil
	})
	api("POST /record", func(w http.ResponseWriter, r *http.Request) error {
		var req struct {
			Seconds int `json:"seconds"`
		}
		if err := decode(r, &req); err != nil {
			return err
		}
		if req.Seconds < 1 || req.Seconds > maxRecordSecs {
			return badRequest(fmt.Sprintf("seconds: от 1 до %d", maxRecordSecs))
		}
		if err := d.startRecording(req.Seconds); err != nil {
			return conflict(err.Error())
		}
		writeJSON(w, d.recordStatus())
		return nil
	})
	api("POST /enroll", func(w http.ResponseWriter, r *http.Request) error {
		var req struct {
			Name string `json:"name"`
		}
		if err := decode(r, &req); err != nil {
			return err
		}
		if _, err := d.startEnroll(req.Name); err != nil {
			return err
		}
		writeJSON(w, d.state().Enroll)
		return nil
	})
	api("DELETE /enroll", func(w http.ResponseWriter, r *http.Request) error {
		d.endEnroll()
		writeOK(w)
		return nil
	})
	api("POST /enroll/ambient", func(w http.ResponseWriter, r *http.Request) error {
		level, err := d.recordAmbient(r.Context())
		if err != nil {
			return err
		}
		writeJSON(w, map[string]float64{"level": level})
		return nil
	})
	api("POST /enroll/takes", func(w http.ResponseWriter, r *http.Request) error {
		res, err := d.recordTake(r.Context())
		if err != nil {
			return err
		}
		writeJSON(w, res)
		return nil
	})
	api("DELETE /enroll/takes/{i}", func(w http.ResponseWriter, r *http.Request) error {
		i, err := strconv.Atoi(r.PathValue("i"))
		if err != nil {
			return notFound("нет такой записи")
		}
		if err := d.deleteTake(i); err != nil {
			return err
		}
		writeJSON(w, d.state().Enroll)
		return nil
	})
	// {i} is a take index or "last" (the latest take even if it was rejected).
	api("GET /enroll/takes/{i}", func(w http.ResponseWriter, r *http.Request) error {
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
			return notFound("нет такой записи")
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(wav.Encode(pcm, kws.SampleRate))
		return nil
	})
	api("POST /enroll/speech", func(w http.ResponseWriter, r *http.Request) error {
		var req struct {
			Seconds float64 `json:"seconds"`
		}
		if err := decode(r, &req); err != nil {
			return err
		}
		level, err := d.recordSpeech(r.Context(), req.Seconds)
		if err != nil {
			return err
		}
		writeJSON(w, map[string]float64{"level": level})
		return nil
	})
	api("POST /enroll/train", func(w http.ResponseWriter, r *http.Request) error {
		var req struct {
			Action *string `json:"action"`
		}
		if err := decode(r, &req); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if err := d.finishEnroll(req.Action); err != nil {
			return err
		}
		writeOK(w)
		return nil
	})
	return d.guard(mux)
}

// guard refuses changes coming from other sites' pages, so a page open in a
// browser in this network cannot drive the speaker, and wakes the event hub
// after a change. Apps do not send Origin.
func (d *Daemon) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		if o := r.Header.Get("Origin"); o != "" {
			if u, err := url.Parse(o); err != nil || u.Host != r.Host {
				writeErr(w, &apiError{http.StatusForbidden, "forbidden", "запрос с чужого сайта"})
				return
			}
		}
		next.ServeHTTP(w, r)
		d.hub.poke()
	})
}

// info is the first request after discovery.
func (d *Daemon) info() map[string]any {
	d.mu.Lock()
	name := d.stationName()
	d.mu.Unlock()
	return map[string]any{"api_version": APIVersion, "name": name, "features": map[string]bool{
		"assistant": d.opt.Assistant != nil, "judge": d.opt.Judge != nil, "led": d.opt.LED != nil,
		"player": d.opt.Actuator != nil, "quick": d.opt.Commands != nil, "volume": d.opt.Volume != nil, "alarms": d.opt.Alarms != nil,
	}}
}

func (d *Daemon) hitByPath(r *http.Request) (*Hit, error) {
	if id, err := strconv.Atoi(r.PathValue("id")); err == nil {
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, h := range d.hits {
			if h.ID == id {
				return h, nil
			}
		}
	}
	return nil, notFound("нет такого срабатывания")
}

// apiError is an error with its place in the contract: HTTP status and a code
// for programs. Any other error from a handler is a bad request.
type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string { return e.message }

func badRequest(msg string) error { return &apiError{http.StatusBadRequest, "bad_request", msg} }
func notFound(msg string) error   { return &apiError{http.StatusNotFound, "not_found", msg} }
func conflict(msg string) error   { return &apiError{http.StatusConflict, "conflict", msg} }
func internal(err error) error {
	return &apiError{http.StatusInternalServerError, "internal", err.Error()}
}

func writeErr(w http.ResponseWriter, err error) {
	e := &apiError{http.StatusBadRequest, "bad_request", err.Error()}
	if errors.Is(err, context.Canceled) {
		e = &apiError{499, "canceled", "запрос отменён"}
	}
	errors.As(err, &e)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.status)
	json.NewEncoder(w).Encode(map[string]map[string]string{"error": {"code": e.code, "message": e.message}})
}

// decode reads a JSON body of a sane size.
func decode(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(v)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeOK(w http.ResponseWriter) { writeJSON(w, map[string]bool{"ok": true}) }
