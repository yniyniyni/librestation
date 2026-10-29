package scene

import (
	"testing"
	"time"

	"voice/internal/led/anim"
	"voice/internal/led/asset"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func newClock() *clock               { return &clock{time.Date(2026, 1, 1, 12, 34, 0, 0, time.UTC)} }
func lit(r anim.Ring) bool {
	for _, c := range r {
		if c != (asset.RGB{}) {
			return true
		}
	}
	return false
}

func flat(g uint8) *asset.Anim {
	f := make([]uint8, asset.ScreenW*asset.ScreenH)
	for i := range f {
		f[i] = g
	}
	return &asset.Anim{Frames: [][]uint8{f}, Delays: []time.Duration{time.Second}}
}

func newScene(a Assets) (*Scene, *clock) {
	c := newClock()
	return New(a, c.now), c
}

func TestIdleShowsClock(t *testing.T) {
	s, c := newScene(Assets{})
	scr, ring := s.Frame()
	if want := anim.Clock(asset.BuiltinFont(), c.now(), 3); scr != want {
		t.Fatal("idle screen is not the clock")
	}
	if lit(ring) {
		t.Fatal("ring must be dark on clock")
	}
}

func TestPlayingShowsSpectrumPausedShowsClock(t *testing.T) {
	s, c := newScene(Assets{})
	var bands [12]float64
	for i := range bands {
		bands[i] = 1
	}
	s.Audio(0.5, bands)
	s.Playing(true)
	scr, ring := s.Frame()
	var peaks [12]float64
	if want := anim.Spectrum(bands, &peaks, 40*time.Millisecond); scr != want {
		t.Fatal("playing screen is not spectrum")
	}
	if lit(ring) {
		t.Fatal("ring must be dark on music")
	}
	s.Playing(false)
	c.add(40 * time.Millisecond)
	scr, _ = s.Frame()
	if scr != anim.Clock(asset.BuiltinFont(), c.now(), 3) {
		t.Fatal("paused must show clock")
	}
}

func TestMusicStyles(t *testing.T) {
	s, c := newScene(Assets{})
	var bands [12]float64
	bands[0] = 0.8
	s.Audio(0, bands)
	s.Playing(true)
	st := DefaultSettings()
	st.Music = "clockbar"
	s.SetSettings(st)
	scr, _ := s.Frame()
	if scr != anim.ClockBar(asset.BuiltinFont(), c.now(), bands) {
		t.Fatal("clockbar mismatch")
	}
	st.Music = "scope"
	s.SetSettings(st)
	scr, _ = s.Frame()
	if scr != anim.Scope(bands, 0) {
		t.Fatal("scope mismatch")
	}
}

func TestVolumeOverlayExpires(t *testing.T) {
	s, c := newScene(Assets{})
	s.Volume(50, 1)
	_, ring := s.Frame()
	if ring != anim.VolumeRing(50) {
		t.Fatal("volume ring expected")
	}
	c.add(1400 * time.Millisecond)
	if _, ring = s.Frame(); ring != anim.VolumeRing(50) {
		t.Fatal("still expected at 1.4s")
	}
	c.add(200 * time.Millisecond)
	if _, ring = s.Frame(); lit(ring) {
		t.Fatal("overlay must be gone at 1.6s")
	}
}

func TestFastKnobLatestWins(t *testing.T) {
	s, c := newScene(Assets{})
	for v := 1; v <= 20; v++ {
		s.Volume(v, 1)
		c.add(40 * time.Millisecond)
	}
	if _, ring := s.Frame(); ring != anim.VolumeRing(20) {
		t.Fatal("latest volume must win")
	}
}

func TestVolumeTimerRestarts(t *testing.T) {
	s, c := newScene(Assets{})
	s.Volume(30, 1)
	c.add(1 * time.Second)
	s.Volume(31, 1)
	c.add(1 * time.Second)
	if _, ring := s.Frame(); ring != anim.VolumeRing(31) {
		t.Fatal("expiry must count from the last Volume")
	}
}

func TestVolumePulseAssetByDirection(t *testing.T) {
	up, down := flat(10), flat(200)
	var pu, pd [11]*asset.Anim
	pu[5], pd[5] = up, down
	s, c := newScene(Assets{Pulse: map[string][11]*asset.Anim{"up": pu, "down": pd}})
	s.Volume(50, 1)
	scr, _ := s.Frame()
	if scr != anim.VolumePulse(up, 0) {
		t.Fatal("up pulse expected")
	}
	c.add(100 * time.Millisecond)
	s.Volume(50, -1)
	scr, _ = s.Frame()
	if scr != anim.VolumePulse(down, 0) {
		t.Fatal("down pulse expected, restarted at t=0")
	}
	s.Volume(50, 0) // keeps previous direction
	scr, _ = s.Frame()
	if scr != anim.VolumePulse(down, 0) {
		t.Fatal("dir 0 keeps previous direction")
	}
	// number setting bypasses the pulse
	st := DefaultSettings()
	st.VolumeScreen = "number"
	s.SetSettings(st)
	scr, _ = s.Frame()
	if scr != anim.VolumeNumber(50) {
		t.Fatal("number expected")
	}
}

func TestCommandOverAIThenBack(t *testing.T) {
	icon := flat(77)
	s, c := newScene(Assets{Icons: map[string]*asset.Anim{"next": icon}})
	s.AI(true)
	s.Command("next")
	scr, ring := s.Frame()
	want, _ := anim.FlashRing("sweep", "next", 0)
	if scr != anim.Icon(icon, 0) || ring != want {
		t.Fatal("command overlay expected over AI")
	}
	c.add(800 * time.Millisecond) // flash over, icon still up: ring dark
	scr, ring = s.Frame()
	if scr != anim.Icon(icon, 800*time.Millisecond) || lit(ring) {
		t.Fatal("icon with dark ring expected")
	}
	c.add(300 * time.Millisecond)
	scr, ring = s.Frame()
	if scr != anim.AIDots(0, 1100*time.Millisecond) || ring != anim.AIRing("carousel", 1100*time.Millisecond) {
		t.Fatal("back to AI expected")
	}
}

func TestVolumeOverAI(t *testing.T) {
	s, c := newScene(Assets{})
	s.AI(true)
	c.add(time.Second)
	s.Volume(40, 1)
	if _, ring := s.Frame(); ring != anim.VolumeRing(40) {
		t.Fatal("volume over AI expected")
	}
	c.add(2 * time.Second)
	if _, ring := s.Frame(); ring != anim.AIRing("carousel", 3*time.Second) {
		t.Fatal("AI expected after volume")
	}
}

func TestNewerOverlayReplacesOlder(t *testing.T) {
	s, _ := newScene(Assets{})
	s.Volume(40, 1)
	s.Command("play")
	want, _ := anim.FlashRing("sweep", "play", 0)
	if _, ring := s.Frame(); ring != want {
		t.Fatal("command must replace volume")
	}
	s.Volume(40, 1)
	if _, ring := s.Frame(); ring != anim.VolumeRing(40) {
		t.Fatal("volume must replace command")
	}
}

func TestAIScreenAndLevel(t *testing.T) {
	s, c := newScene(Assets{})
	s.AI(true)
	s.Audio(0.7, [12]float64{})
	c.add(500 * time.Millisecond)
	scr, _ := s.Frame()
	if scr != anim.AIDots(0.7, 500*time.Millisecond) {
		t.Fatal("dots with level expected")
	}
	st := DefaultSettings()
	st.AIScreen = "eq"
	s.SetSettings(st)
	scr, _ = s.Frame()
	if scr != anim.AIEq(0.7, 500*time.Millisecond) {
		t.Fatal("eq expected")
	}
}

func TestAITimeoutIsCallers(t *testing.T) {
	s, c := newScene(Assets{})
	s.AI(true)
	c.add(10 * time.Minute)
	if _, ring := s.Frame(); !lit(ring) {
		t.Fatal("scene must not time out AI")
	}
	s.AI(false)
	if _, ring := s.Frame(); lit(ring) {
		t.Fatal("AI off must show clock")
	}
}

func TestMissingAssetsFallbacks(t *testing.T) {
	s, _ := newScene(Assets{})
	s.Volume(33, -1)
	if scr, _ := s.Frame(); scr != anim.VolumeNumber(33) {
		t.Fatal("number fallback expected")
	}
	s.Command("pause")
	want, _ := anim.FlashRing("sweep", "pause", 0)
	scr, ring := s.Frame()
	if ring != want || scr != (anim.Screen{}) {
		t.Fatal("blank screen with ring flash expected")
	}
}

func TestValidateRejectsUnknown(t *testing.T) {
	if err := DefaultSettings().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mut := range []func(*Settings){
		func(s *Settings) { s.VolumeScreen = "x" },
		func(s *Settings) { s.AIRing = "x" },
		func(s *Settings) { s.AIScreen = "x" },
		func(s *Settings) { s.Flash = "x" },
		func(s *Settings) { s.Music = "x" },
	} {
		s := DefaultSettings()
		mut(&s)
		if s.Validate() == nil {
			t.Fatalf("%+v must be rejected", s)
		}
	}
}

func TestBrightnessSetting(t *testing.T) {
	if DefaultSettings().Brightness != 0 {
		t.Fatal("default brightness must be automatic (0)")
	}
	for _, b := range []int{-1, 101} {
		st := DefaultSettings()
		st.Brightness = b
		if st.Validate() == nil {
			t.Fatalf("brightness %d accepted", b)
		}
	}
	st := DefaultSettings()
	st.Brightness = 40
	if err := st.Validate(); err != nil {
		t.Fatal(err)
	}
	sc := New(Assets{}, time.Now)
	sc.SetSettings(st)
	if sc.Brightness() != 40 {
		t.Fatalf("Brightness() = %d, want 40", sc.Brightness())
	}
}
