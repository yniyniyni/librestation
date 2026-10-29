package anim

import (
	"math"
	"strconv"
	"time"

	"voice/internal/led/asset"
)

// Screen is the 25x16 monochrome display, row-major, one gray byte per pixel.
type Screen [asset.ScreenW * asset.ScreenH]uint8

const (
	sw, sh    = asset.ScreenW, asset.ScreenH
	clockGray = 200
)

func (s *Screen) put(x, y int, g uint8) {
	if x >= 0 && x < sw && y >= 0 && y < sh {
		s[y*sw+x] = g
	}
}

func (s *Screen) disc(cx, cy, r float64, g uint8) {
	for y := 0; y < sh; y++ {
		for x := 0; x < sw; x++ {
			dx, dy := float64(x)-cx, float64(y)-cy
			if dx*dx+dy*dy <= r*r {
				s[y*sw+x] = g
			}
		}
	}
}

func (s *Screen) glyph(g *[asset.GlyphW * asset.GlyphH]bool, x0, y0 int, gray uint8) {
	for p, on := range g {
		if on {
			s.put(x0+p%asset.GlyphW, y0+p/asset.GlyphW, gray)
		}
	}
}

// luma is the gray of a color: 0.3R+0.59G+0.11B.
func luma(c rgbF) uint8 { return uint8(round(0.3*c[0] + 0.59*c[1] + 0.11*c[2])) }

func (s *Screen) clock(f *asset.Font, now time.Time, y0 int) {
	digits := [4]int{now.Hour() / 10, now.Hour() % 10, now.Minute() / 10, now.Minute() % 10}
	xs := [4]int{0, 6, 14, 20}
	for j, d := range digits {
		s.glyph(&f.Digits[d], xs[j], y0, clockGray)
	}
	if now.Second()%2 == 0 {
		s.glyph(&f.Colon, 12, y0, clockGray)
	}
}

// Clock draws HH:MM with the stock font; the colon shows on even seconds.
func Clock(f *asset.Font, now time.Time, y0 int) Screen {
	var s Screen
	s.clock(f, now, y0)
	return s
}

// VolumeNumber draws the volume percentage centered in the built-in 5x9 font.
func VolumeNumber(v int) Screen {
	var s Screen
	f := asset.BuiltinFont()
	str := strconv.Itoa(max(0, min(100, v)))
	x0 := (sw - (len(str)*6 - 1)) / 2
	for j, ch := range str {
		s.glyph(&f.Digits[ch-'0'], x0+j*6, 3, 255)
	}
	return s
}

func fromAnim(a *asset.Anim, t time.Duration) Screen {
	var s Screen
	if a != nil {
		copy(s[:], a.At(t))
	}
	return s
}

// VolumePulse shows the stock volume pulse animation frame at t.
func VolumePulse(a *asset.Anim, t time.Duration) Screen { return fromAnim(a, t) }

// Icon shows an animation frame at t (e.g. a media command icon).
func Icon(a *asset.Anim, t time.Duration) Screen { return fromAnim(a, t) }

// AIDots is four bouncing dots in Google colors (as gray); level (0..1) is
// the voice loudness that makes them jump higher.
func AIDots(level float64, t time.Duration) Screen {
	var s Screen
	ts := t.Seconds()
	v := clamp01(level)
	for i, c := range google {
		h := (2 + 3*v) * math.Max(0, math.Sin(ts*6-float64(i)*0.9))
		s.disc(4+float64(i)*5.6, 9-h, 1.9, luma(c))
	}
	return s
}

// AIEq is eight voice-driven equalizer bars, taller in the middle.
func AIEq(level float64, t time.Duration) Screen {
	var s Screen
	ts := t.Seconds()
	v := clamp01(level)
	for i := 0; i < 8; i++ {
		fi := float64(i)
		x := 1 + i*3
		env := 1 - math.Abs(fi-3.5)/4.5
		h := max(1, round((1.5+12*v*env)*(0.7+0.3*math.Sin(ts*9+fi*2.3))))
		g := luma(google[i/2])
		for y := 0; y < h; y++ {
			s.put(x, 15-y, g)
			s.put(x+1, 15-y, g)
		}
	}
	return s
}

// peakFall is how far a spectrum peak falls per 40 ms.
const peakFall = 0.25

// Spectrum draws 12 bars (2 px apart, bands 0..1) with falling peak dots.
// peaks is caller-owned state that Spectrum updates; dt is the time since
// the previous frame.
func Spectrum(bands [12]float64, peaks *[12]float64, dt time.Duration) Screen {
	var s Screen
	fall := peakFall * float64(dt) / float64(40*time.Millisecond)
	for i, b := range bands {
		h := round(clamp01(b) * 15)
		p := math.Max(peaks[i]-fall, float64(h))
		peaks[i] = p
		for y := 0; y < h; y++ {
			s.put(i*2, 15-y, uint8(70+y*12))
		}
		s.put(i*2, 15-round(p), 255)
	}
	return s
}

// Scope draws a running sound-wave line across the middle; amplitude comes
// from the low bands.
func Scope(bands [12]float64, t time.Duration) Screen {
	var s Screen
	ts := t.Seconds()
	amp := (bands[0] + bands[1] + bands[4]) / 3
	k := uint8(math.Max(0, math.Min(255, float64(round(140+115*amp)))))
	for x := 0; x < sw; x++ {
		fx := float64(x)
		y := 7.5 + amp*7*math.Sin(fx*0.7+ts*9)*math.Sin(fx*0.23+ts*2.3)
		s.put(x, max(0, min(15, round(y))), k)
	}
	return s
}

// ClockBar draws the clock (y0=1) over a 1..3 row, 25-column spectrum strip.
func ClockBar(f *asset.Font, now time.Time, bands [12]float64) Screen {
	var s Screen
	s.clock(f, now, 1)
	for x := 0; x < sw; x++ {
		v := bands[x*12/sw]
		h := 1
		if v > 0.6 {
			h = 3
		} else if v > 0.3 {
			h = 2
		}
		for y := 0; y < h; y++ {
			s.put(x, 15-y, uint8(120+45*y))
		}
	}
	return s
}
