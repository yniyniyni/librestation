// Package daemon runs on the station: it listens to the processed mic
// stream, detects the user's words, opens an Alice voice request on a hit,
// and serves the web UI that records and trains new words.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"voice/internal/alarms"

	miccap "voice/internal/capture"
	"voice/internal/dsp"
	"voice/internal/judge"
	"voice/internal/kws"
	"voice/internal/led/proto"
	"voice/internal/wav"
	"voice/internal/ycmd"
)

type Options struct {
	DataDir   string // words, settings
	Negatives string // bundled negatives.bin
	Listen    string
	MicCmd    []string    // capture helper command
	MicMuted  func() bool // reads the station's mic switch; nil = no switch
	Soloist   string      // Soloist WebSocket URL (used by the real Actuator)
	Actuator  Actuator    // runs word actions; nil = actions are only logged
	// SetEQ applies the EQ mode on the device; nil = no EQ control.
	SetEQ func(mode int) error
	// EQPresets lists the available EQ presets for the UI.
	EQPresets []EQPreset
	// Ring is the alarm or timer ringing, which any word stops first; nil = none.
	Ring RingControl
	// Alarms is what the panel sets and shows; nil = none.
	Alarms AlarmsControl
	// FindMusic chooses an alarm's music when it is set; nil = searched at the ring.
	FindMusic func(ctx context.Context, query, kind string) (alarms.Music, error)
	// Assistant runs the "assistant" word action and gets every audio block;
	// nil = that action is only logged.
	Assistant Assistant
	// AssistantStatus is the session's state for the panel; SpotifyLinked
	// says whether Spotify is linked, known false until it has been checked.
	// Both must return at once; nil = none.
	AssistantStatus func() any
	SpotifyLinked   func() (linked, known bool)
	// PiperMissing is true while Piper cannot run (replies use Gemini's
	// voice). It must return at once; nil = never.
	PiperMissing func() bool
	// Volume is the one volume (volume.Sync); nil = none.
	Volume VolumeControl
	// LED serves /api/v1/led* (a proxy to ledd); nil = none.
	LED http.Handler
	// OnLED receives events for the LED daemon (nil = off). It must not block.
	OnLED func(proto.Event)
	// Judge labels hits with an audio model (nil = off); JudgeModel names it
	// for the UI.
	Judge      Judge
	JudgeModel string
	// Commands is Yandex's quick-command model, run on channel 0 of the raw
	// capture (nil = none); CommandsErr says why there is none.
	Commands    *ycmd.Model
	CommandsErr string
	// LiveModel names the Gemini Live model for the panel.
	LiveModel string
	// Wake are Yandex's stock trigger words, run on channel 0 when switched on
	// in the panel; WakeErr says why there are none.
	Wake    []WakeModel
	WakeErr string
}

// WakeModel is one stock trigger word.
type WakeModel struct {
	ID, Name string
	Model    *ycmd.Model
}

// RingControl is the ring as the words see it. Both calls run on the audio
// goroutine, under d.mu: they must not block or call back into the daemon.
type RingControl interface {
	Ringing() bool
	Stop()
}

// stopRingLocked ends the ring if one is on and says so; caller holds d.mu. The
// same word said again just after must not pause the music the ring gives back.
func (d *Daemon) stopRingLocked() bool {
	if d.opt.Ring == nil || !d.opt.Ring.Ringing() {
		return false
	}
	d.opt.Ring.Stop()
	d.lastAction["pause"] = d.now()
	return true
}

// VolumeControl is the volume behind the API. Its calls may wait for Soloist,
// so they are never made under d.mu.
type VolumeControl interface {
	Volume() int
	KnobStep() int
	SetKnobStep(n int) error
	Step(delta int) error
}

// Judge is the audio model that labels hits (judge.Client).
type Judge interface {
	Judge(ctx context.Context, h judge.Hit) (judge.Verdict, error)
}

// Assistant is the voice assistant session (assistant.Session). Its methods
// never block and never call back into the daemon under its own locks.
type Assistant interface {
	// Audio takes every block of frontend output.
	Audio([]int16)
	// Wake starts or interrupts a request; onNoRequest is called from the
	// session's goroutine when the request held nothing for the speaker.
	Wake(ctx context.Context, onNoRequest func())
	// Busy is true while a request is running.
	Busy() bool
	// Hush stops the reply and goes idle; from idle it does nothing.
	Hush()
	// State is idle|listening|thinking|speaking|followup.
	State() string
	// SpokeAt is when the reply last stopped playing.
	SpokeAt() time.Time
	// LiveAudio takes 16 kHz channel 0 of the raw capture (x LiveGain) for
	// the Live session.
	LiveAudio([]int16)
}

// Settings is persisted in <data>/settings.json.
type Settings struct {
	DryRun        bool                     `json:"dry_run"`        // log hits without acting
	ActivationOff bool                     `json:"activation_off"` // master switch: all detection off
	Words         map[string]*WordSettings `json:"words"`
	// JudgeOff stops sending hits to the audio model even with a key.
	JudgeOff bool `json:"judge_off"`
	// JudgeModel is the model that judges hits; "" = Options.JudgeModel.
	JudgeModel string `json:"judge_model,omitempty"`
	// AutoFalseUndo marks hits whose action the user took back as false
	// alarms without asking.
	AutoFalseUndo bool `json:"auto_false_undo"`
	// EQMode is the Amlogic audio HAL's eq_mode (0-9); -1 = not set.
	EQMode int `json:"eq_mode"`
	// Assistant is read by the session on every turn.
	Assistant AssistantSettings `json:"assistant"`
	// Quick switches Yandex's quick commands.
	Quick QuickSettings `json:"quick"`
	// StockWake are the stock trigger words switched on, by id.
	StockWake map[string]bool `json:"stock_wake,omitempty"`
}

// QuickSettings turn Yandex's quick commands off, all or by group (pause,
// next, prev, volume, play). Everything is on by default.
type QuickSettings struct {
	Off      bool            `json:"off"`
	Disabled map[string]bool `json:"disabled,omitempty"`
}

// AssistantSettings are the assistant's choices in the panel.
type AssistantSettings struct {
	Model     string `json:"model,omitempty"` // the agent's, when Live is out; "" = the judge's current model
	TTS       string `json:"tts"`             // "gemini" (default) or "piper"
	Voice     string `json:"voice,omitempty"` // Gemini voice; "" = tts.DefaultVoice
	City      string `json:"city,omitempty"`  // weather's default city
	EarconOff bool   `json:"earcon_off"`      // no activation beep
}

// maxAssistantText bounds the voice and city, in characters.
const maxAssistantText = 64

// Settings returns a copy of the settings.
func (d *Daemon) Settings() Settings {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.settings
}

// AssistantSettings returns a copy of the assistant's settings.
func (d *Daemon) AssistantSettings() AssistantSettings {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.settings.Assistant
	if s.TTS == "" {
		s.TTS = "gemini"
	}
	return s
}

// WakeWords are the names of the enabled words that call the assistant.
func (d *Daemon) WakeWords() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var names []string
	for _, m := range d.opt.Wake {
		if d.settings.StockWake[m.ID] {
			names = append(names, m.Name)
		}
	}
	for _, w := range d.words {
		if s := d.settings.Words[w.ID]; s != nil && s.Enabled && s.Action == "assistant" {
			names = append(names, w.Model.Name)
		}
	}
	return names
}

// AssistantModel is the model for the assistant's next turn: its own choice,
// or the judge's current one.
func (d *Daemon) AssistantModel() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if m := d.settings.Assistant.Model; m != "" {
		return m
	}
	return d.assistantDefaultModelLocked()
}

// assistantDefaultModelLocked is what an empty assistant model means; caller
// holds d.mu.
func (d *Daemon) assistantDefaultModelLocked() string {
	if m := d.judgeModelLocked(); m != "" {
		return m
	}
	return judge.DefaultModel
}

type WordSettings struct {
	Enabled     bool    `json:"enabled"`
	Sensitivity float32 `json:"sensitivity"` // threshold offset, -0.3..0.3
	Action      string  `json:"action"`      // one of actionNames; "" = none
}

type word struct {
	ID    string
	Model *kws.Model
}

// Hit is one detection, kept for the UI (with audio for false-alarm marking).
type Hit struct {
	ID     int       `json:"id"`
	Word   string    `json:"word"`
	Name   string    `json:"name"`
	Score  float32   `json:"score"`
	At     time.Time `json:"at"`
	Action string    `json:"action,omitempty"`
	Fired  bool      `json:"fired"` // the word's action was run
	// Debounced: the same action ran moments ago, so this hit was only logged.
	Debounced bool   `json:"debounced,omitempty"`
	Error     string `json:"error,omitempty"`
	Playback  bool   `json:"playback,omitempty"` // music was playing
	// Judge is the audio model's call; JudgeError why there is none.
	Judge      *judge.Verdict `json:"judge,omitempty"`
	JudgeError string         `json:"judge_error,omitempty"`
	Undone     float64        `json:"undone,omitempty"` // seconds until the user took the action back
	// Verdict is "false" or "ok" once decided; Auto when the station
	// decided it. FalseFile is the clip saved as the word's hard negative.
	Verdict   string `json:"verdict,omitempty"`
	Auto      bool   `json:"auto,omitempty"`
	FalseFile string `json:"false_file,omitempty"`
	// Source is "yandex" for Yandex's quick commands ("" = our word); their
	// Word and Name are the command.
	Source string `json:"source,omitempty"`

	audio     []int16
	volBefore int  // volume when the hit fired, -1 unknown
	volMoved  bool // the volume has moved the action's way since
	pruned    bool // dropped from the list: its files go
}

type Daemon struct {
	opt Options
	ctx context.Context // Run's context; nil before Run

	onActionDone func()             // test hook
	retrain      func(string) error // test hook for autoRetrain; nil = startTraining

	mu       sync.Mutex
	settings Settings
	words    []word
	det      *kws.Detector
	detWords []string // word id per detector model
	paused   int      // >0 while enrolling: no detection
	ring     *ring
	level    float64 // dBFS of the last chunk
	audioOK  bool
	audioErr string
	capture  *capture
	hits     []*Hit
	hitSeq   int
	enroll   *enrollment
	train    trainState
	name     cachedName

	now        func() time.Time // injectable clock
	lastAction map[string]time.Time

	enrollIdle    time.Duration // abandon an enrollment after this long without API calls
	enrollTimer   *time.Timer
	enrollTouched time.Time

	rec recorder

	judgeQ     chan *Hit      // hits waiting for the audio model
	judgeDelay time.Duration  // wait before judging, so a take-back is known
	io         sync.WaitGroup // background hit writes
	ioMu       sync.Mutex     // serialises hit writes; taken before mu
	settingsIO sync.Mutex     // serialises settings writes; taken before mu
	falses     map[string]falseCount
	hub        eventHub
	playback   string // last Soloist status
	volume     int    // last observed volume, -1 unknown

	bands   *dsp.Bands // playback reference spectrum; nil without OnLED
	lastLED time.Time  // last audio event sent (audio goroutine only)

	spot        spotter     // nil without Options.Commands (audio goroutine only)
	yin         *ycmd.Input // the spotter's input (audio goroutine only)
	wakes       []*wakeSpot // the stock trigger words (audio goroutine only)
	lastWake    time.Time   // when one of them last fired; under mu
	yring       *ring       // channel 0 at 16 kHz (x1), for hit clips
	started     time.Time   // when New ran
	lastPlaying time.Time   // when music was last seen playing
}

// actionDebounce is how long the same action is ignored after it ran: several
// models may map to one action and one utterance can trigger more than one.
const actionDebounce = 1500 * time.Millisecond

// enrollIdleTimeout is how long an abandoned enrollment keeps detection paused.
const enrollIdleTimeout = 10 * time.Minute

// cachedName is the app-given station name, re-read when maind rewrites
// the synced config (the owner renamed the station).
type cachedName struct {
	name  string
	mtime time.Time
}

// stationName returns the station's name; caller holds d.mu.
func (d *Daemon) stationName() string {
	// TODO(task 7): real station name; hostname for now.
	if d.name.name != "" {
		return d.name.name
	}
	h, _ := os.Hostname()
	return h
}

type trainState struct {
	Active bool    `json:"active"`
	Word   string  `json:"word,omitempty"` // id
	Name   string  `json:"name,omitempty"`
	Stage  string  `json:"stage,omitempty"`
	Frac   float64 `json:"frac"`
	Error  string  `json:"error,omitempty"`
	Done   string  `json:"done,omitempty"` // id of the last trained word
}

const ringSecs = 12

func New(opt Options) (*Daemon, error) {
	if err := os.MkdirAll(filepath.Join(opt.DataDir, "words"), 0o755); err != nil {
		return nil, err
	}
	d := &Daemon{opt: opt, ring: newRing(ringSecs * kws.SampleRate),
		now: time.Now, lastAction: map[string]time.Time{}, enrollIdle: enrollIdleTimeout, volume: -1,
		yring: newRing(hitSecs * kws.SampleRate)}
	d.started = d.now()
	if opt.Commands != nil {
		d.spot, d.yin = ycmd.NewSpotter(opt.Commands), ycmd.NewInput()
	}
	for _, m := range opt.Wake {
		sp := ycmd.NewSpotter(m.Model)
		sp.Dither(wakeDither)
		d.wakes = append(d.wakes, &wakeSpot{id: m.ID, name: m.Name, spot: sp, reset: sp.Reset})
	}
	d.settings = Settings{Words: map[string]*WordSettings{}}
	if b, err := os.ReadFile(d.settingsPath()); err == nil {
		if err := json.Unmarshal(b, &d.settings); err != nil {
			// A broken file must not keep the panel and the alarms down.
			log.Printf("settings: %v; kept as .bad, starting from the defaults", err)
			os.Rename(d.settingsPath(), d.settingsPath()+".bad")
			d.settings = Settings{}
		}
	}
	if d.settings.Words == nil {
		d.settings.Words = map[string]*WordSettings{}
	}
	if opt.OnLED != nil {
		d.bands = dsp.NewBands(miccap.Rate)
	}
	d.judgeQ, d.judgeDelay = make(chan *Hit, judgeQueue), undoWindow
	if err := d.loadHits(); err != nil {
		return nil, err
	}
	if opt.Commands != nil {
		d.migrateStockWords()
	}
	if err := d.loadWords(); err != nil {
		return nil, err
	}
	return d, nil
}

// removeWord deletes a word's settings and recordings and reloads the words.
func (d *Daemon) removeWord(id string) error {
	d.mu.Lock()
	delete(d.settings.Words, id)
	d.mu.Unlock()
	d.persistSettings()
	if err := os.RemoveAll(d.wordDir(id)); err != nil {
		return err
	}
	return d.loadWords()
}

// migrateStockWords removes enrolled words that duplicate a stock command
// of the loaded Yandex model, by name or by dir. A failure is only logged:
// it must not keep voiced from starting.
func (d *Daemon) migrateStockWords() {
	dirs, err := os.ReadDir(filepath.Join(d.opt.DataDir, "words"))
	if err != nil {
		return // loadWords reports it
	}
	for _, e := range dirs {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if b, err := os.ReadFile(filepath.Join(d.wordDir(name), "name.txt")); err == nil && strings.TrimSpace(string(b)) != "" {
			name = strings.TrimSpace(string(b))
		}
		if !stockCommandName(name) && !stockCommandName(e.Name()) {
			continue
		}
		if err := d.removeWord(e.Name()); err != nil {
			log.Printf("слово «%s» совпадает со штатной командой колонки, но удалить его не вышло: %v", name, err)
			continue
		}
		log.Printf("слово «%s» совпадает со штатной командой колонки — удалено", name)
	}
}

func (d *Daemon) settingsPath() string { return filepath.Join(d.opt.DataDir, "settings.json") }
func (d *Daemon) wordDir(id string) string {
	return filepath.Join(d.opt.DataDir, "words", id)
}

// persistSettings writes settings without holding d.mu over the disk, because
// the audio goroutine needs d.mu. Writes are serialised and each one takes a
// fresh snapshot, so a later state is never overwritten by an earlier one.
func (d *Daemon) persistSettings() error {
	d.settingsIO.Lock()
	defer d.settingsIO.Unlock()
	d.mu.Lock()
	b, _ := json.MarshalIndent(d.settings, "", "  ")
	d.mu.Unlock()
	tmp := d.settingsPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, d.settingsPath())
}

func (d *Daemon) loadWords() error {
	dirs, err := os.ReadDir(filepath.Join(d.opt.DataDir, "words"))
	if err != nil {
		return err
	}
	// Disk first, d.mu after: the audio goroutine needs d.mu.
	var words []word
	falses := map[string]falseCount{}
	for _, e := range dirs {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(d.wordDir(e.Name()), "model.json"))
		if err != nil {
			continue // enrolled but never trained
		}
		m, err := kws.ReadModel(b)
		if err != nil {
			log.Printf("word %s: bad model: %v", e.Name(), err)
			continue
		}
		words = append(words, word{e.Name(), m})
		falses[e.Name()] = d.countFalses(e.Name())
	}
	sort.Slice(words, func(i, j int) bool { return words[i].ID < words[j].ID })
	d.mu.Lock()
	defer d.mu.Unlock()
	d.words, d.falses = words, falses
	for _, w := range words {
		if d.settings.Words[w.ID] == nil {
			d.settings.Words[w.ID] = &WordSettings{Enabled: true}
		}
	}
	return d.rebuildDetector()
}

// rebuildDetector applies the enabled words; caller holds d.mu.
func (d *Daemon) rebuildDetector() error {
	var models []*kws.Model
	var sens []float32
	d.detWords = nil
	for _, w := range d.words {
		s := d.settings.Words[w.ID]
		if s == nil || !s.Enabled {
			continue
		}
		models = append(models, w.Model)
		sens = append(sens, -s.Sensitivity) // higher sensitivity = lower threshold
		d.detWords = append(d.detWords, w.ID)
	}
	if len(models) == 0 {
		d.det = nil
		return nil
	}
	det, err := kws.NewDetector(models, sens)
	if err != nil {
		return err
	}
	d.det = det
	return nil
}

// Run captures and processes audio until ctx ends. Yandex's voice stack is
// killed first (and again whenever the mic reads silence) so we get the mic.
func (d *Daemon) Run(ctx context.Context) {
	d.mu.Lock()
	d.ctx = ctx
	d.mu.Unlock()
	go d.runJudge(ctx)
	if len(d.opt.MicCmd) == 0 {
		d.mu.Lock()
		d.audioErr = "no capture command"
		d.mu.Unlock()
		log.Printf("audio: no capture command configured")
		<-ctx.Done()
		return
	}
	killYandex()
	r := &miccap.Runner{
		Cmd:       d.opt.MicCmd,
		OnBlock:   func(b []int16) { d.onRaw(b); d.onAudio(d.spotCommands(b)) },
		OnSilence: killYandex,
		Muted:     d.opt.MicMuted,
		OnMuted:   d.micMuted,
	}
	err := r.Run(ctx)
	d.mu.Lock()
	d.audioOK = false
	d.audioErr = fmt.Sprint(err)
	d.mu.Unlock()
}

// micMuted notes that no audio comes while the switch is on; the next block
// clears it.
func (d *Daemon) micMuted() {
	d.mu.Lock()
	d.audioOK, d.audioErr = false, "микрофон выключен кнопкой"
	d.mu.Unlock()
}

// onAudio takes a block of frontend output (16 kHz mono, gain applied).
func (d *Daemon) onAudio(pcm []int16) {
	level := d.process(pcm)
	d.sendAudioLED(level)
}

// wakeLocked calls the assistant for hit h. The session drives the LEDs; the
// hit is neither judged nor debounced.
func (d *Daemon) wakeLocked(h *Hit) {
	if asst := d.opt.Assistant; asst != nil && !d.settings.DryRun {
		h.Fired = true
		ctx := d.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		asst.Wake(ctx, func() { d.assistantNoRequest(h) })
	}
	d.addHitLocked(h)
}

// Playing says whether music is playing as far as Soloist has told.
func (d *Daemon) Playing() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.playback == "playing"
}

// sendAudioLED sends the level and the playback spectrum to ledd, at most
// once per ledAudioEvery. It runs on the audio goroutine without d.mu.
func (d *Daemon) sendAudioLED(dbfs float64) {
	if d.opt.OnLED == nil {
		return
	}
	now := d.now()
	if !d.lastLED.IsZero() && now.Sub(d.lastLED) < ledAudioEvery {
		return
	}
	d.lastLED = now
	lv := d.bands.Levels()
	d.opt.OnLED(proto.Event{T: "audio", Level: min(max((dbfs+60)/50, 0), 1), Bands: lv[:]})
}

// ledAudioEvery is the minimum gap between audio events sent to ledd.
const ledAudioEvery = 40 * time.Millisecond

// process handles one block of frontend output and returns its level in dBFS.
func (d *Daemon) process(pcm []int16) float64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	var e float64
	for _, s := range pcm {
		e += float64(s) * float64(s)
	}
	d.level = 10*math.Log10(e/float64(max(len(pcm), 1))+1e-9) - 90.3 // dBFS
	d.audioOK, d.audioErr = true, ""
	d.ring.write(pcm)
	if a := d.opt.Assistant; a != nil {
		a.Audio(pcm)
	}
	if c := d.capture; c != nil {
		c.buf = append(c.buf, pcm...)
		if len(c.buf) >= c.want {
			close(c.done)
			d.capture = nil
		}
	}
	if d.det == nil || d.paused > 0 || d.settings.ActivationOff {
		return d.level
	}
	d.det.Push(pcm, func(i int, score float32) { d.onHit(d.detWords[i], score) })
	return d.level
}

// onHit records a detection and runs the word's action off the audio
// goroutine; caller holds d.mu.
func (d *Daemon) onHit(id string, score float32) {
	h := &Hit{Word: id, Name: d.wordName(id), Score: score, At: d.now(), volBefore: d.volume,
		Playback: d.playback == "playing"}
	h.audio = d.ring.last(hitSecs * kws.SampleRate)
	var action string
	if s := d.settings.Words[id]; s != nil {
		action = s.Action
	}
	h.Action = action
	asst := d.opt.Assistant
	if d.stopRingLocked() && action != "assistant" {
		h.Action, h.Fired = "ring_stop", true
		d.addHitLocked(h)
		log.Printf("hit %s score %.3f stopped the ring", id, score)
		return
	}
	if action == "assistant" {
		d.wakeLocked(h)
		log.Printf("hit %s score %.3f action %q", id, score, action)
		return
	}
	if asst != nil && action != "" && asst.Busy() {
		h.Debounced = true // quick commands wait while the assistant talks
		action = ""
	}
	if action != "" && !d.settings.DryRun {
		d.actLocked(h, action)
	}
	d.addHitLocked(h)
	d.judgeLaterLocked(h)
	log.Printf("hit %s score %.3f action %q fired %v debounced %v", id, score, action, h.Fired, h.Debounced)
}

// actLocked runs action for the new hit h off the audio goroutine, unless the
// same action ran within actionDebounce. "hush" stops the assistant's reply;
// the rest go to the Actuator. Caller holds d.mu and adds h after.
func (d *Daemon) actLocked(h *Hit, action string) {
	if t, ok := d.lastAction[action]; ok && d.now().Sub(t) < actionDebounce {
		h.Debounced = true
		return
	}
	h.Fired = true
	d.lastAction[action] = d.now()
	d.revertedLocked(action) // this command may take back the previous one
	switch action {
	case "pause", "play", "next", "prev":
		if d.opt.OnLED != nil {
			d.opt.OnLED(proto.Event{T: "cmd", A: action})
		}
	}
	act, asst := d.opt.Actuator, d.opt.Assistant
	go func() {
		var err error
		if action == "hush" {
			asst.Hush() // takes its own locks: never under d.mu
		} else {
			err = runAction(act, action)
		}
		if err != nil {
			log.Printf("action %s: %v", action, err)
			d.mu.Lock()
			h.Error = err.Error()
			d.mu.Unlock()
			d.persist(h)
		}
		d.actionDone()
	}()
}

// assistantNoRequest marks the hit that woke the assistant as a false alarm:
// no request followed. Runs on the session's goroutine, without its locks.
func (d *Daemon) assistantNoRequest(h *Hit) {
	d.mu.Lock()
	d.markFalseLocked(h, true)
	d.mu.Unlock()
	d.persist(h)
}

// actionDone is a test hook called after each action finished.
func (d *Daemon) actionDone() {
	if d.onActionDone != nil {
		d.onActionDone()
	}
}

func (d *Daemon) wordName(id string) string {
	for _, w := range d.words {
		if w.ID == id {
			return w.Model.Name
		}
	}
	return id
}

// capture records the next n samples (plus pre samples already in the
// ring) from the live stream.
type capture struct {
	buf  []int16
	want int
	done chan struct{}
}

var errBusy = conflict("идёт другая запись")

func (d *Daemon) record(ctx context.Context, secs, preSecs float64) ([]int16, error) {
	pre := int(preSecs * kws.SampleRate)
	d.mu.Lock()
	if d.capture != nil {
		d.mu.Unlock()
		return nil, errBusy
	}
	if !d.audioOK {
		d.mu.Unlock()
		return nil, conflict("нет звука со станции: " + d.audioErr)
	}
	c := &capture{want: pre + int(secs*kws.SampleRate), done: make(chan struct{})}
	c.buf = d.ring.last(pre)
	d.capture = c
	d.mu.Unlock()
	select {
	case <-c.done:
		return c.buf[:c.want], nil
	case <-ctx.Done():
		d.mu.Lock()
		if d.capture == c {
			d.capture = nil
		}
		d.mu.Unlock()
		return nil, ctx.Err()
	case <-time.After(time.Duration(secs+5) * time.Second):
		d.mu.Lock()
		if d.capture == c {
			d.capture = nil
		}
		d.mu.Unlock()
		return nil, conflict("звук со станции пропал")
	}
}

var slugRe = regexp.MustCompile(`[^a-z0-9а-яё]+`)

// wordID derives a directory name from the word's spelling.
func wordID(name string) string {
	s := slugRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "word"
	}
	return s
}

func readWAVs(paths []string) ([][]int16, error) {
	var out [][]int16
	for _, p := range paths {
		pcm, err := wav.ReadFile(p, kws.SampleRate)
		if err != nil {
			return nil, err
		}
		out = append(out, pcm)
	}
	return out, nil
}
