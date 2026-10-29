// Package daemon runs on the station: it listens to the processed mic
// stream, detects the user's words, opens an Alice voice request on a hit,
// and serves the web UI that records and trains new words.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	miccap "voice/internal/capture"
	"voice/internal/dsp"
	"voice/internal/frontend"
	"voice/internal/kws"
	"voice/internal/led/proto"
	"voice/internal/wav"
)

type Options struct {
	DataDir   string // words, settings
	Negatives string // bundled negatives.bin
	Listen    string
	MicCmd    []string // capture helper command
	Soloist   string   // Soloist WebSocket URL (used by the real Actuator)
	Actuator  Actuator // runs word actions; nil = actions are only logged
	// OnLED receives events for the LED daemon (nil = off). It must not block.
	OnLED func(proto.Event)
}

// Settings is persisted in <data>/settings.json.
type Settings struct {
	Gain   float64                  `json:"gain"`    // frontend output gain
	DryRun bool                     `json:"dry_run"` // log hits without acting
	Words  map[string]*WordSettings `json:"words"`
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
	ID    int       `json:"id"`
	Word  string    `json:"word"`
	Name  string    `json:"name"`
	Score float32   `json:"score"`
	At    time.Time `json:"at"`
	Fired bool      `json:"fired"` // the word's action was run
	// Debounced: the same action ran moments ago, so this hit was only logged.
	Debounced bool   `json:"debounced,omitempty"`
	Error     string `json:"error,omitempty"`
	audio     []int16
}

type Daemon struct {
	opt Options

	onActionDone func() // test hook

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

	bands   *dsp.Bands // playback reference spectrum; nil without OnLED
	lastLED time.Time  // last audio event sent (audio goroutine only)
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
		now: time.Now, lastAction: map[string]time.Time{}, enrollIdle: enrollIdleTimeout}
	d.settings = Settings{Gain: 32, Words: map[string]*WordSettings{}}
	if b, err := os.ReadFile(d.settingsPath()); err == nil {
		if err := json.Unmarshal(b, &d.settings); err != nil {
			return nil, fmt.Errorf("settings: %w", err)
		}
	}
	if d.settings.Words == nil {
		d.settings.Words = map[string]*WordSettings{}
	}
	if opt.OnLED != nil {
		d.bands = dsp.NewBands(miccap.Rate)
	}
	if err := d.loadWords(); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *Daemon) settingsPath() string { return filepath.Join(d.opt.DataDir, "settings.json") }
func (d *Daemon) wordDir(id string) string {
	return filepath.Join(d.opt.DataDir, "words", id)
}

// saveSettings writes settings atomically; caller holds d.mu.
func (d *Daemon) saveSettings() error {
	b, _ := json.MarshalIndent(d.settings, "", "  ")
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
	d.mu.Lock()
	defer d.mu.Unlock()
	d.words = nil
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
		d.words = append(d.words, word{e.Name(), m})
		if d.settings.Words[e.Name()] == nil {
			d.settings.Words[e.Name()] = &WordSettings{Enabled: true}
		}
	}
	sort.Slice(d.words, func(i, j int) bool { return d.words[i].ID < d.words[j].ID })
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
	if len(d.opt.MicCmd) == 0 {
		d.mu.Lock()
		d.audioErr = "no capture command"
		d.mu.Unlock()
		log.Printf("audio: no capture command configured")
		<-ctx.Done()
		return
	}
	cfg := frontend.DefaultConfig()
	d.mu.Lock()
	if d.settings.Gain > 0 {
		cfg.Gain = d.settings.Gain
	}
	d.mu.Unlock()
	fe := frontend.New(cfg)
	killYandex()
	r := &miccap.Runner{
		Cmd:       d.opt.MicCmd,
		OnBlock:   func(b []int16) { d.onRaw(b); d.onAudio(fe.Process(b)) },
		OnSilence: killYandex,
	}
	err := r.Run(ctx)
	d.mu.Lock()
	d.audioOK = false
	d.audioErr = fmt.Sprint(err)
	d.mu.Unlock()
}

// onAudio takes a block of frontend output (16 kHz mono, gain applied).
func (d *Daemon) onAudio(pcm []int16) {
	level := d.process(pcm)
	d.sendAudioLED(level)
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
	if c := d.capture; c != nil {
		c.buf = append(c.buf, pcm...)
		if len(c.buf) >= c.want {
			close(c.done)
			d.capture = nil
		}
	}
	if d.det == nil || d.paused > 0 {
		return d.level
	}
	d.det.Push(pcm, func(i int, score float32) { d.onHit(d.detWords[i], score) })
	return d.level
}

// onHit records a detection and runs the word's action off the audio
// goroutine; caller holds d.mu.
func (d *Daemon) onHit(id string, score float32) {
	h := &Hit{Word: id, Name: d.wordName(id), Score: score, At: time.Now()}
	h.audio = d.ring.last(3 * kws.SampleRate)
	var action string
	if s := d.settings.Words[id]; s != nil {
		action = s.Action
	}
	if t, ok := d.lastAction[action]; ok && action != "" && !d.settings.DryRun && d.now().Sub(t) < actionDebounce {
		h.Debounced = true
		action = ""
	}
	if action != "" && !d.settings.DryRun {
		h.Fired = true
		d.lastAction[action] = d.now()
		switch action {
		case "pause", "play", "next", "prev":
			if d.opt.OnLED != nil {
				d.opt.OnLED(proto.Event{T: "cmd", A: action})
			}
		}
		act := d.opt.Actuator
		go func() {
			if err := runAction(act, action); err != nil {
				log.Printf("action %s: %v", action, err)
				d.mu.Lock()
				h.Error = err.Error()
				d.mu.Unlock()
			}
			d.actionDone()
		}()
	}
	d.hitSeq++
	h.ID = d.hitSeq
	d.hits = append(d.hits, h)
	if len(d.hits) > 20 {
		d.hits = d.hits[len(d.hits)-20:]
	}
	log.Printf("hit %s score %.3f action %q debounced %v", id, score, action, h.Debounced)
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

var errBusy = errors.New("another recording is in progress")

func (d *Daemon) record(ctx context.Context, secs, preSecs float64) ([]int16, error) {
	pre := int(preSecs * kws.SampleRate)
	d.mu.Lock()
	if d.capture != nil {
		d.mu.Unlock()
		return nil, errBusy
	}
	if !d.audioOK {
		d.mu.Unlock()
		return nil, errors.New("no audio from the station: " + d.audioErr)
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
		return nil, errors.New("audio stream stalled")
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
