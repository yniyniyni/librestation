// Package asset loads the stock Station Max LED assets: GIF animations for
// the 25x16 screen, the 5x9 clock font and .led ring patterns.
package asset

import (
	"bufio"
	"fmt"
	"image"
	"image/gif"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// ScreenW and ScreenH are the screen dimensions in pixels.
	ScreenW, ScreenH = 25, 16
	// GlyphW and GlyphH are the clock font glyph dimensions.
	GlyphW, GlyphH = 5, 9

	defaultDelay = 100 * time.Millisecond
	litThreshold = 100
	ringLEDs     = 24
)

// Anim is a composited grayscale animation; each frame is W*H bytes, row-major.
type Anim struct {
	Frames [][]uint8
	Delays []time.Duration
}

// At returns the frame shown d after the start; past the end it is the last frame.
func (a *Anim) At(d time.Duration) []uint8 {
	if len(a.Frames) == 0 {
		return nil
	}
	var t time.Duration
	for i, dl := range a.Delays {
		t += dl
		if d < t && i < len(a.Frames) {
			return a.Frames[i]
		}
	}
	return a.Frames[len(a.Frames)-1]
}

// LoadGIF decodes a GIF onto a 25x16 black canvas.
func LoadGIF(path string) (*Anim, error) {
	return loadGray(path, ScreenW, ScreenH)
}

// loadGray decodes path, drawing every frame at (0,0) onto a w*h canvas and
// honouring disposal. Transparent pixels leave the canvas untouched.
func loadGray(path string, w, h int) (*Anim, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	g, err := gif.DecodeAll(f)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	canvas := make([]uint8, w*h)
	a := &Anim{}
	for i, img := range g.Image {
		var prev []uint8
		disposal := byte(gif.DisposalNone)
		if i < len(g.Disposal) {
			disposal = g.Disposal[i]
		}
		if disposal == gif.DisposalPrevious {
			prev = append([]uint8(nil), canvas...)
		}
		b := img.Bounds()
		for y := b.Min.Y; y < b.Max.Y && y < h; y++ {
			for x := b.Min.X; x < b.Max.X && x < w; x++ {
				if x < 0 || y < 0 {
					continue
				}
				r, gg, bb, al := img.At(x, y).RGBA()
				if al == 0 {
					continue
				}
				// Premultiplied 16-bit; the stock files are opaque or fully transparent.
				canvas[y*w+x] = uint8((30*(r>>8) + 59*(gg>>8) + 11*(bb>>8)) / 100)
			}
		}
		a.Frames = append(a.Frames, append([]uint8(nil), canvas...))
		d := defaultDelay
		if i < len(g.Delay) && g.Delay[i] > 0 {
			d = time.Duration(g.Delay[i]) * 10 * time.Millisecond
		}
		a.Delays = append(a.Delays, d)

		switch disposal {
		case gif.DisposalBackground:
			rect := image.Rectangle{Min: image.Pt(max(b.Min.X, 0), max(b.Min.Y, 0)), Max: image.Pt(min(b.Max.X, w), min(b.Max.Y, h))}
			for y := rect.Min.Y; y < rect.Max.Y; y++ {
				for x := rect.Min.X; x < rect.Max.X; x++ {
					canvas[y*w+x] = 0
				}
			}
		case gif.DisposalPrevious:
			copy(canvas, prev)
		}
	}
	if len(a.Frames) == 0 {
		return nil, fmt.Errorf("%s: no frames", path)
	}
	return a, nil
}

// Font holds the 5x9 clock glyphs, row-major, true = lit.
type Font struct {
	Colon  [GlyphW * GlyphH]bool
	Digits [10][GlyphW * GlyphH]bool
}

// LoadClockFont reads the stock clock font GIF: frame 0 is the colon,
// frames 1..10 are digits 0..9.
func LoadClockFont(path string) (*Font, error) {
	a, err := loadGray(path, GlyphW, GlyphH)
	if err != nil {
		return nil, err
	}
	if len(a.Frames) < 11 {
		return nil, fmt.Errorf("%s: font needs 11 frames, got %d", path, len(a.Frames))
	}
	lit := func(frame []uint8) (g [GlyphW * GlyphH]bool) {
		for i, v := range frame {
			g[i] = v > litThreshold
		}
		return
	}
	f := &Font{Colon: lit(a.Frames[0])}
	for d := range f.Digits {
		f.Digits[d] = lit(a.Frames[d+1])
	}
	return f, nil
}

// Stock glyph shapes, one string per glyph: rows joined by '|', '#' = lit.
var (
	builtinColon  = ".....|.....|#....|.....|.....|.....|#....|.....|....."
	builtinDigits = [10]string{
		".###.|#...#|#...#|#...#|#...#|#...#|#...#|#...#|.###.",
		"..#..|.##..|..#..|..#..|..#..|..#..|..#..|..#..|.###.",
		".###.|#...#|....#|....#|..##.|.#...|#....|#....|#####",
		".###.|#...#|....#|....#|.###.|....#|....#|#...#|.###.",
		"#...#|#...#|#...#|#...#|.####|....#|....#|....#|....#",
		"#####|#....|#....|####.|....#|....#|....#|#...#|.###.",
		".###.|#...#|#....|#....|####.|#...#|#...#|#...#|.###.",
		"#####|....#|....#|...#.|...#.|..#..|..#..|..#..|..#..",
		".###.|#...#|#...#|#...#|.###.|#...#|#...#|#...#|.###.",
		".###.|#...#|#...#|#...#|.####|....#|....#|#...#|.###.",
	}
)

func parseGlyph(s string) (g [GlyphW * GlyphH]bool) {
	i := 0
	for _, c := range s {
		switch c {
		case '#':
			g[i] = true
			i++
		case '.':
			i++
		}
	}
	return
}

// BuiltinFont returns the stock clock font shapes without touching the disk.
func BuiltinFont() *Font {
	f := &Font{Colon: parseGlyph(builtinColon)}
	for d, s := range builtinDigits {
		f.Digits[d] = parseGlyph(s)
	}
	return f
}

// RGB is one ring LED color.
type RGB struct{ R, G, B uint8 }

// RingPattern is a parsed .led file for the 24-LED ring.
type RingPattern struct {
	Frames [][ringLEDs]RGB
	Delays []time.Duration
	Loop   bool
}

// parseGradient reads "P C P C ... ms".
func parseGradient(f []string) (fr [ringLEDs]RGB, d time.Duration, ok bool) {
	if len(f) < 5 || len(f)%2 != 1 {
		return fr, 0, false
	}
	type stop struct {
		at float64
		c  RGB
	}
	var stops []stop
	for i := 0; i+1 < len(f); i += 2 {
		at, err := strconv.ParseFloat(f[i], 64)
		v, err2 := strconv.ParseUint(f[i+1], 16, 32)
		if err != nil || err2 != nil || len(f[i+1]) != 6 {
			return fr, 0, false
		}
		stops = append(stops, stop{at, RGB{uint8(v >> 16), uint8(v >> 8), uint8(v)}})
	}
	ms, err := strconv.Atoi(f[len(f)-1])
	if err != nil || ms < 0 {
		return fr, 0, false
	}
	for i := range fr {
		x := float64(i) / (ringLEDs - 1)
		j := 0
		for j+2 < len(stops) && x > stops[j+1].at {
			j++
		}
		a, b := stops[j], stops[j+1]
		t := 0.0
		if b.at > a.at {
			t = max(0, min(1, (x-a.at)/(b.at-a.at)))
		}
		mix := func(p, q uint8) uint8 { return uint8(float64(p) + (float64(q)-float64(p))*t + 0.5) }
		fr[i] = RGB{mix(a.c.R, b.c.R), mix(a.c.G, b.c.G), mix(a.c.B, b.c.B)}
	}
	return fr, time.Duration(ms) * time.Millisecond, true
}

// ParseLED parses a .led pattern. Blank lines, '#' comments, "smooth N" and
// "gammacorrection" are skipped, as is the unsupported "rotate" directive and a
// "gradient" of another form; "gradient P C P C ms" is a frame of colors C at
// positions P along the ring; "loop" sets Loop; every other line is 24 hex
// colors followed by a duration in ms.
func ParseLED(r io.Reader) (*RingPattern, error) {
	p := &RingPattern{}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		switch fields[0] {
		case "loop":
			p.Loop = true
			continue
		case "smooth", "gammacorrection":
			continue
		case "gradient":
			if fr, d, ok := parseGradient(fields[1:]); ok {
				p.Frames = append(p.Frames, fr)
				p.Delays = append(p.Delays, d)
			}
			continue
		case "rotate":
			continue // a generator directive in some stock files; not a frame, not supported
		}
		if len(fields) != ringLEDs+1 {
			return nil, fmt.Errorf("line %d: want %d colors and a duration, got %d fields", n, ringLEDs, len(fields))
		}
		var fr [ringLEDs]RGB
		for i := 0; i < ringLEDs; i++ {
			v, err := strconv.ParseUint(fields[i], 16, 32)
			if err != nil || len(fields[i]) != 6 {
				return nil, fmt.Errorf("line %d: bad color %q", n, fields[i])
			}
			fr[i] = RGB{uint8(v >> 16), uint8(v >> 8), uint8(v)}
		}
		ms, err := strconv.Atoi(fields[ringLEDs])
		if err != nil || ms < 0 {
			return nil, fmt.Errorf("line %d: bad duration %q", n, fields[ringLEDs])
		}
		p.Frames = append(p.Frames, fr)
		p.Delays = append(p.Delays, time.Duration(ms)*time.Millisecond)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(p.Frames) == 0 {
		return nil, fmt.Errorf("no frames")
	}
	return p, nil
}
