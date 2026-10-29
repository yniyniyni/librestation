package anim

import (
	"testing"
	"time"

	"voice/internal/led/asset"
)

var (
	blue   = asset.RGB{R: 66, G: 133, B: 244}
	red    = asset.RGB{R: 234, G: 67, B: 53}
	yellow = asset.RGB{R: 251, G: 188, B: 5}
	green  = asset.RGB{R: 52, G: 168, B: 83}
	off    asset.RGB
)

func TestVolumeRingStock(t *testing.T) {
	if r := VolumeRing(0); r != (Ring{}) {
		t.Fatalf("v=0 not dark: %v", r)
	}
	r := VolumeRing(50)
	for i := 0; i < 12; i++ {
		if r[i] != (asset.RGB{R: 0x9A, G: 0xFF, B: 0x45}) {
			t.Fatalf("v=50 led %d = %v", i, r[i])
		}
	}
	if r[12] != off {
		t.Fatalf("v=50 led 12 = %v, want off", r[12])
	}
	r = VolumeRing(100)
	for i := 0; i < 23; i++ {
		if r[i] != (asset.RGB{R: 0xE3, G: 0x09, B: 0x00}) {
			t.Fatalf("v=100 led %d = %v", i, r[i])
		}
	}
	if r[23] != off {
		t.Fatalf("v=100 led 23 = %v, want off", r[23])
	}
}

func TestCarouselAtZero(t *testing.T) {
	r := AIRing("carousel", 0)
	for i, want := range []asset.RGB{blue, red, yellow, green} {
		for j := 0; j < 6; j++ {
			if got := r[i*6+j]; got != want {
				t.Fatalf("led %d = %v, want %v", i*6+j, got, want)
			}
		}
	}
}

func TestAIRingStylesLit(t *testing.T) {
	for _, st := range []string{"carousel", "flow", "stock"} {
		r := AIRing(st, 300*time.Millisecond)
		lit := 0
		for _, c := range r {
			if c != off {
				lit++
			}
		}
		if lit == 0 {
			t.Errorf("%s: nothing lit", st)
		}
	}
	// stock arc is centered on LED 0
	r := AIRing("stock", 0)
	if r[0] == off || r[1] == off || r[23] == off || r[12] != off {
		t.Fatalf("stock arc not centered on 0: %v", r)
	}
}

func brightest(r Ring) int {
	best, bi := -1, 0
	for i, c := range r {
		if s := int(c.R) + int(c.G) + int(c.B); s > best {
			best, bi = s, i
		}
	}
	return bi
}

func TestSweepNextGoesClockwise(t *testing.T) {
	a, _ := FlashRing("sweep", "next", 100*time.Millisecond)
	b, _ := FlashRing("sweep", "next", 300*time.Millisecond)
	if !(brightest(a) < brightest(b)) {
		t.Fatalf("next: %d !< %d", brightest(a), brightest(b))
	}
	a, _ = FlashRing("sweep", "prev", 100*time.Millisecond)
	b, _ = FlashRing("sweep", "prev", 300*time.Millisecond)
	if !(brightest(a) > brightest(b)) {
		t.Fatalf("prev: %d !> %d", brightest(a), brightest(b))
	}
}

func TestSweepPauseConvergesToFront(t *testing.T) {
	r, _ := FlashRing("sweep", "pause", 500*time.Millisecond)
	if brightest(r) != 0 {
		t.Fatalf("pause end brightest = %d, want 0", brightest(r))
	}
	r, _ = FlashRing("sweep", "play", 0)
	if brightest(r) != 0 {
		t.Fatalf("play start brightest = %d, want 0", brightest(r))
	}
}

func TestSweepEnds(t *testing.T) {
	for _, c := range []struct {
		style string
		dur   time.Duration
	}{{"sweep", 600 * time.Millisecond}, {"white", 400 * time.Millisecond}, {"color", 600 * time.Millisecond}} {
		if _, ok := FlashRing(c.style, "next", c.dur-time.Millisecond); !ok {
			t.Errorf("%s: finished early", c.style)
		}
		r, ok := FlashRing(c.style, "next", c.dur)
		if ok || r != (Ring{}) {
			t.Errorf("%s: not finished at %v", c.style, c.dur)
		}
	}
	if _, ok := FlashRing("sweep", "next", 700*time.Millisecond); ok {
		t.Fatal("ok at 0.7s")
	}
}

func TestFlashWhiteAndColor(t *testing.T) {
	r, _ := FlashRing("white", "pause", 0)
	if r[5] != (asset.RGB{R: 255, G: 255, B: 255}) {
		t.Fatalf("white start = %v", r[5])
	}
	r, _ = FlashRing("color", "pause", 0)
	if r[3] != (asset.RGB{R: 255, G: 170, B: 0}) {
		t.Fatalf("color start = %v", r[3])
	}
	r, _ = FlashRing("color", "pause", 200*time.Millisecond)
	if r[3] != off {
		t.Fatalf("color off phase = %v", r[3])
	}
}

func lit(s Screen, x, y int) bool { return s[y*asset.ScreenW+x] != 0 }

func TestClockLayout(t *testing.T) {
	f := asset.BuiltinFont()
	even := time.Date(2026, 1, 1, 12, 34, 0, 0, time.UTC)
	const y0 = 3
	s := Clock(f, even, y0)
	digits := [4]int{1, 2, 3, 4}
	for j, x0 := range [4]int{0, 6, 14, 20} {
		for p, on := range f.Digits[digits[j]] {
			x, y := x0+p%5, y0+p/5
			if lit(s, x, y) != on {
				t.Fatalf("digit %d pixel (%d,%d) = %v, want %v", j, x, y, lit(s, x, y), on)
			}
			if on && s[y*25+x] != 200 {
				t.Fatalf("gray = %d", s[y*25+x])
			}
		}
	}
	for y := 0; y < 16; y++ {
		if lit(s, 5, y) {
			t.Fatalf("column 5 lit at row %d", y)
		}
	}
	colon := func(s Screen) (n int) {
		for p, on := range f.Colon {
			if on && lit(s, 12+p%5, y0+p/5) {
				n++
			}
		}
		return
	}
	if colon(s) == 0 {
		t.Fatal("colon off at even second")
	}
	if colon(Clock(f, even.Add(time.Second), y0)) != 0 {
		t.Fatal("colon on at odd second")
	}
}

func litRange(s Screen) (minX, maxX int, n int) {
	minX, maxX = 99, -1
	for i, v := range s {
		if v != 0 {
			n++
			minX, maxX = min(minX, i%25), max(maxX, i%25)
		}
	}
	return
}

func TestVolumeNumberCentered(t *testing.T) {
	lo, hi, n := litRange(VolumeNumber(7))
	if n == 0 || lo < 10 || hi > 14 {
		t.Fatalf("v=7 spans %d..%d (n=%d)", lo, hi, n)
	}
	lo, hi, _ = litRange(VolumeNumber(100))
	if lo < 3 || hi > 21 || hi-lo < 15 {
		t.Fatalf("v=100 spans %d..%d", lo, hi)
	}
}

func TestSpectrumHeights(t *testing.T) {
	var full, zero [12]float64
	for i := range full {
		full[i] = 1
	}
	var peaks [12]float64
	s := Spectrum(full, &peaks, 40*time.Millisecond)
	for i := 0; i < 12; i++ {
		for y := 0; y < 16; y++ {
			if !lit(s, i*2, y) {
				t.Fatalf("band %d row %d dark", i, y)
			}
			if i < 11 && lit(s, i*2+1, y) {
				t.Fatalf("odd column %d lit", i*2+1)
			}
		}
	}
	// silence: the peaks fall by <= 0.25 px / 40 ms
	Spectrum(zero, &peaks, 40*time.Millisecond)
	if peaks[0] != 15-0.25 {
		t.Fatalf("peak = %v, want 14.75", peaks[0])
	}
	Spectrum(zero, &peaks, 80*time.Millisecond)
	if peaks[0] != 14.25 {
		t.Fatalf("peak after 80ms = %v, want 14.25", peaks[0])
	}
	var p2 [12]float64
	Spectrum(zero, &p2, 40*time.Millisecond)
	for _, v := range p2 {
		if v != 0 {
			t.Fatalf("peak below zero: %v", v)
		}
	}
	s = Spectrum(zero, &p2, 40*time.Millisecond)
	for i, v := range s {
		if v != 0 && (i/25 != 15 || i%25%2 != 0) {
			t.Fatalf("silent screen has pixel %d", i)
		}
	}
}

func topLit(s Screen) int {
	for i, v := range s {
		if v != 0 {
			return i / 25
		}
	}
	return 99
}

func TestAIDotsLevelRaisesJump(t *testing.T) {
	top := func(level float64) int {
		best := 99
		for ms := 0; ms <= 1000; ms += 10 {
			best = min(best, topLit(AIDots(level, time.Duration(ms)*time.Millisecond)))
		}
		return best
	}
	if hi, lo := top(1), top(0); hi >= lo {
		t.Fatalf("level 1 top row %d not above level 0 top row %d", hi, lo)
	}
}

func TestAIEqLevelRaisesBars(t *testing.T) {
	if topLit(AIEq(1, 0)) >= topLit(AIEq(0, 0)) {
		t.Fatal("level 1 bars not taller than level 0")
	}
}

func TestScopeAndClockBarAndAnim(t *testing.T) {
	var b [12]float64
	for i := range b {
		b[i] = 0.8
	}
	if _, _, n := litRange(Scope(b, 123*time.Millisecond)); n != 25 {
		t.Fatalf("scope pixels = %d, want 25", n)
	}
	f := asset.BuiltinFont()
	s := ClockBar(f, time.Date(2026, 1, 1, 9, 5, 0, 0, time.UTC), b)
	for x := 0; x < 25; x++ {
		if s[15*25+x] != 120 || s[14*25+x] != 165 || s[13*25+x] != 210 {
			t.Fatalf("bar column %d wrong: %d %d %d", x, s[15*25+x], s[14*25+x], s[13*25+x])
		}
	}
	a := &asset.Anim{Frames: [][]uint8{make([]uint8, 400), make([]uint8, 400)}, Delays: []time.Duration{time.Second, time.Second}}
	a.Frames[1][7] = 9
	if Icon(a, 1500*time.Millisecond)[7] != 9 || VolumePulse(a, 0)[7] != 0 {
		t.Fatal("anim frame selection wrong")
	}
	if Icon(nil, 0) != (Screen{}) {
		t.Fatal("nil anim not blank")
	}
}
