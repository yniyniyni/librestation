// Package scene keeps the LED UI state and decides which frame to show.
package scene

import (
	"fmt"
	"math"
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
	aiState   string // listening, thinking, speaking, error
	aiStart   time.Time
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

	switch s.overlay {
	case overlayVolume:
		if t := now.Sub(s.ovStart); t < volumeHold {
			return s.volumeFrame(t)
		}
		s.overlay = overlayNone
	case overlayCommand:
		t := now.Sub(s.ovStart)
		ring, ok := anim.FlashRing(s.set.Flash, s.action, t)
		if ok || t < commandHold {
			return s.iconScreen(t), ring
		}
		s.overlay = overlayNone
	}

	switch {
	case s.ai:
		t := now.Sub(s.aiStart)
		if s.aiState == "error" && t >= AIErrorHold {
			s.ai = false
			break
		}
		var scr anim.Screen
		if s.set.AIScreen == "eq" {
			scr = anim.AIEq(s.level, t)
		} else {
			scr = anim.AIDots(s.level, t)
		}
		return scr, s.aiRing(t)
	}
	if s.playing {
		return s.musicScreen(now, dt), anim.Ring{}
	}
	return anim.Clock(s.font, now, 3), anim.Ring{}
}

// aiRing draws the ring for the current assistant state; t is the time
// since the state began.
func (s *Scene) aiRing(t time.Duration) anim.Ring {
	ring := anim.AIRing(s.set.AIRing, t)
	switch s.aiState {
	case "thinking": // the listening ring, one revolution per second
		var r anim.Ring
		k := int(t.Seconds()*float64(len(r))) % len(r)
		for i := range r {
			r[(i+k)%len(r)] = ring[i]
		}
		return r
	case "speaking": // brightness follows the level
		g := 0.25 + 0.75*math.Max(0, math.Min(1, s.level))
		for i, c := range ring {
			ring[i] = asset.RGB{R: uint8(float64(c.R) * g), G: uint8(float64(c.G) * g), B: uint8(float64(c.B) * g)}
		}
	case "error": // red blink, 4 Hz
		ring = anim.Ring{}
		if int(t/(125*time.Millisecond))%2 == 0 {
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
