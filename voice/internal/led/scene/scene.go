// Package scene keeps the LED UI state and decides which frame to show.
package scene

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"voice/internal/led/anim"
	"voice/internal/led/asset"
)

// Settings selects the visual style of each element.
type Settings struct {
	VolumeScreen string `json:"volume_screen"` // "pulse" | "number"
	AIRing       string `json:"ai_ring"`       // "carousel" | "flow" | "stock"
	AIScreen     string `json:"ai_screen"`     // "dots" | "eq"
	Flash        string `json:"flash"`         // "sweep" | "white" | "color"
	Music        string `json:"music"`         // "spectrum" | "scope" | "clockbar"
	// Brightness of the screen: 0 = automatic (light sensor), 1..100 = fixed.
	Brightness int `json:"brightness"`
}

// DefaultSettings returns the default styles.
func DefaultSettings() Settings {
	return Settings{VolumeScreen: "pulse", AIRing: "carousel", AIScreen: "dots", Flash: "sweep", Music: "spectrum"}
}

func oneOf(name, v string, allowed ...string) error {
	for _, a := range allowed {
		if v == a {
			return nil
		}
	}
	return fmt.Errorf("scene: unknown %s %q", name, v)
}

// Validate reports the first unknown style value.
func (s Settings) Validate() error {
	if err := oneOf("volume_screen", s.VolumeScreen, "pulse", "number"); err != nil {
		return err
	}
	if err := oneOf("ai_ring", s.AIRing, "carousel", "flow", "stock"); err != nil {
		return err
	}
	if err := oneOf("ai_screen", s.AIScreen, "dots", "eq"); err != nil {
		return err
	}
	if err := oneOf("flash", s.Flash, "sweep", "white", "color"); err != nil {
		return err
	}
	if err := oneOf("music", s.Music, "spectrum", "scope", "clockbar"); err != nil {
		return err
	}
	if s.Brightness < 0 || s.Brightness > 100 {
		return fmt.Errorf("scene: brightness %d out of 0..100", s.Brightness)
	}
	return nil
}

// Assets are the optional on-device animations; nil entries are allowed.
type Assets struct {
	Font  *asset.Font
	Pulse map[string][11]*asset.Anim // "up", "down", indexed by round(volume/10)
	Icons map[string]*asset.Anim     // pause, play, next, prev
}

const (
	volumeHold  = 1500 * time.Millisecond
	commandHold = time.Second
	// AIErrorHold is how long the AI error state shows before it ends.
	AIErrorHold = 1500 * time.Millisecond
	defaultDT   = 40 * time.Millisecond
	// aiFade is the assistant's way in and out, aiStateFade from one of its
	// states to another: none of them just replaces what was on.
	aiFade      = 350 * time.Millisecond
	aiStateFade = 200 * time.Millisecond
	// speakTau is how fast the speaking ring follows the voice: without it the
	// ring flickers with every syllable.
	speakTau = 120 * time.Millisecond
)

type overlayKind int

const (
	overlayNone overlayKind = iota
	overlayVolume
	overlayCommand
)

// Scene is safe for concurrent use.
type Scene struct {
	mu     sync.Mutex
	assets Assets
	font   *asset.Font
	now    func() time.Time
	set    Settings

	ai        bool
	aiState   string    // listening, thinking, speaking, error
	aiOn      time.Time // when the layer came on; its motion runs from here
	aiStart   time.Time // when the state began
	speakLv   float64   // the voice level the speaking ring shows
	playing   bool
	playStart time.Time
	level     float64
	bands     [12]float64
	peaks     [12]float64

	overlay overlayKind
	ovStart time.Time
	volume  int
	dir     string
	action  string

	lastFrame time.Time

	layer     string // what the last frame showed
	shown     bool   // a frame has been drawn
	lastScr   anim.Screen
	lastRing  anim.Ring
	fadeScr   anim.Screen // what the fade started from
	fadeRing  anim.Ring
	fadeStart time.Time
	fadeLen   time.Duration // 0 = no fade running
}

// New creates a scene with default settings. now is the clock source.
func New(a Assets, now func() time.Time) *Scene {
	f := a.Font
	if f == nil {
		f = asset.BuiltinFont()
	}
	return &Scene{assets: a, font: f, now: now, set: DefaultSettings(), dir: "up"}
}

// Volume shows the volume overlay; dir>0 up, <0 down, 0 keeps the previous direction.
func (s *Scene) Volume(v, dir int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.overlay, s.ovStart, s.volume = overlayVolume, s.now(), v
	if dir > 0 {
		s.dir = "up"
	} else if dir < 0 {
		s.dir = "down"
	}
}

// Command shows the media-command overlay (pause, play, next, prev).
func (s *Scene) Command(action string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.overlay, s.ovStart, s.action = overlayCommand, s.now(), action
}

// Audio stores the latest voice level and spectrum bands.
func (s *Scene) Audio(level float64, bands [12]float64) {
	s.mu.Lock()
	s.level, s.bands = level, bands
	s.mu.Unlock()
}

// AI switches the assistant layer and its state: "listening" (also ""),
// "thinking", "speaking" or "error". Only the error state times itself out.
func (s *Scene) AI(on bool, state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state == "" {
		state = "listening"
	}
	if on && !s.ai {
		s.aiOn = s.now()
	}
	if on && (!s.ai || state != s.aiState) {
		s.aiStart = s.now()
	}
	s.ai, s.aiState = on, state
}

// Playing switches the music layer.
func (s *Scene) Playing(p bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p && !s.playing {
		s.playStart = s.now()
	}
	s.playing = p
}

// SetSettings replaces the styles.
func (s *Scene) SetSettings(st Settings) {
	s.mu.Lock()
	s.set = st
	s.mu.Unlock()
}

// Brightness is the configured screen brightness: 0 = automatic, 1..100 = fixed.
func (s *Scene) Brightness() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.set.Brightness
}

// Frame returns the screen and ring to show now.
func (s *Scene) Frame() (anim.Screen, anim.Ring) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	dt := defaultDT
	if !s.lastFrame.IsZero() && now.After(s.lastFrame) {
		dt = now.Sub(s.lastFrame)
	}
	s.lastFrame = now

	scr, ring, layer := s.compose(now, dt)
	if layer != s.layer {
		if d := fadeFor(s.layer, layer); d > 0 && s.shown {
			s.fadeScr, s.fadeRing, s.fadeStart, s.fadeLen = s.lastScr, s.lastRing, now, d
		} else {
			s.fadeLen = 0
		}
		s.layer = layer
	}
	if s.fadeLen > 0 {
		if p := float64(now.Sub(s.fadeStart)) / float64(s.fadeLen); p >= 1 {
			s.fadeLen = 0
		} else {
			p = p * p * (3 - 2*p) // eased at both ends
			scr, ring = mixScreen(s.fadeScr, scr, p), mixRing(s.fadeRing, ring, p)
		}
	}
	s.lastScr, s.lastRing, s.shown = scr, ring, true
	return scr, ring
}

// fadeFor is how long the change between two layers fades. The overlays answer
// a knob or a command, so they come on at once.
func fadeFor(from, to string) time.Duration {
	fromAI, toAI := strings.HasPrefix(from, "ai:"), strings.HasPrefix(to, "ai:")
	switch {
	case to == "volume" || to == "command":
		return 0
	case fromAI && toAI:
		return aiStateFade
	case fromAI || toAI:
		return aiFade
	}
	return 0
}

func mix8(a, b uint8, p float64) uint8 {
	return uint8(float64(a) + (float64(b)-float64(a))*p + 0.5)
}

func mixScreen(a, b anim.Screen, p float64) anim.Screen {
	for i := range a {
		a[i] = mix8(a[i], b[i], p)
	}
	return a
}

func mixRing(a, b anim.Ring, p float64) anim.Ring {
	for i := range a {
		a[i] = asset.RGB{R: mix8(a[i].R, b[i].R, p), G: mix8(a[i].G, b[i].G, p), B: mix8(a[i].B, b[i].B, p)}
	}
	return a
}

// compose draws the layer on top now and names it.
func (s *Scene) compose(now time.Time, dt time.Duration) (anim.Screen, anim.Ring, string) {
	switch s.overlay {
	case overlayVolume:
		if t := now.Sub(s.ovStart); t < volumeHold {
			scr, ring := s.volumeFrame(t)
			return scr, ring, "volume"
		}
		s.overlay = overlayNone
	case overlayCommand:
		t := now.Sub(s.ovStart)
		ring, ok := anim.FlashRing(s.set.Flash, s.action, t)
		if ok || t < commandHold {
			return s.iconScreen(t), ring, "command"
		}
		s.overlay = overlayNone
	}

	switch {
	case s.ai:
		if s.aiState == "error" && now.Sub(s.aiStart) >= AIErrorHold {
			s.ai = false
			break
		}
		t := now.Sub(s.aiOn)
		var scr anim.Screen
		if s.set.AIScreen == "eq" {
			scr = anim.AIEq(s.level, t)
		} else {
			scr = anim.AIDots(s.level, t)
		}
		return scr, s.aiRing(t, now.Sub(s.aiStart), dt), "ai:" + s.aiState
	}
	if s.playing {
		return s.musicScreen(now, dt), anim.Ring{}, "music"
	}
	return anim.Clock(s.font, now, 3), anim.Ring{}, "clock"
}

// aiRing draws the ring for the current assistant state; t is the time since
// the assistant came on, so a new state carries on the motion, and st since the
// state began.
func (s *Scene) aiRing(t, st, dt time.Duration) anim.Ring {
	ring := anim.AIRing(s.set.AIRing, t)
	switch s.aiState {
	case "thinking": // the listening ring, one revolution per second more
		var r anim.Ring
		k := int(st.Seconds()*float64(len(r))) % len(r)
		for i := range r {
			r[(i+k)%len(r)] = ring[i]
		}
		return r
	case "speaking": // brightness follows the level
		s.speakLv += (s.level - s.speakLv) * (1 - math.Exp(-float64(dt)/float64(speakTau)))
		g := 0.25 + 0.75*math.Max(0, math.Min(1, s.speakLv))
		for i, c := range ring {
			ring[i] = asset.RGB{R: uint8(float64(c.R) * g), G: uint8(float64(c.G) * g), B: uint8(float64(c.B) * g)}
		}
	case "error": // red blink, 4 Hz
		ring = anim.Ring{}
		if int(st/(125*time.Millisecond))%2 == 0 {
			for i := range ring {
				ring[i] = asset.RGB{R: 255}
			}
		}
	}
	return ring
}

func (s *Scene) volumeFrame(t time.Duration) (anim.Screen, anim.Ring) {
	ring := anim.VolumeRing(s.volume)
	if s.set.VolumeScreen == "pulse" {
		n := max(0, min(10, (s.volume+5)/10))
		if a := s.assets.Pulse[s.dir][n]; a != nil {
			return anim.VolumePulse(a, t), ring
		}
	}
	return anim.VolumeNumber(s.volume), ring
}

func (s *Scene) iconScreen(t time.Duration) anim.Screen {
	if a := s.assets.Icons[s.action]; a != nil {
		return anim.Icon(a, t)
	}
	return anim.Screen{}
}

func (s *Scene) musicScreen(now time.Time, dt time.Duration) anim.Screen {
	switch s.set.Music {
	case "scope":
		return anim.Scope(s.bands, now.Sub(s.playStart))
	case "clockbar":
		return anim.ClockBar(s.font, now, s.bands)
	}
	return anim.Spectrum(s.bands, &s.peaks, dt)
}
