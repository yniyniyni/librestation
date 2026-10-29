package asset

import (
	"image"
	"image/color"
	"image/gif"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeGIF(t *testing.T, g *gif.GIF) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "a.gif")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := gif.EncodeAll(f, g); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadGIFGrayAndDelays(t *testing.T) {
	pal := color.Palette{color.RGBA{0, 0, 0, 255}, color.RGBA{255, 255, 255, 255}, color.RGBA{0, 128, 0, 255}}
	f0 := image.NewPaletted(image.Rect(0, 0, 25, 16), pal)
	for i := range f0.Pix {
		f0.Pix[i] = 1
	}
	f1 := image.NewPaletted(image.Rect(0, 0, 25, 16), pal)
	f1.SetColorIndex(3, 2, 2)
	p := writeGIF(t, &gif.GIF{Image: []*image.Paletted{f0, f1}, Delay: []int{10, 10}})

	a, err := LoadGIF(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Frames) != 2 || len(a.Frames[0]) != 400 {
		t.Fatalf("frames=%d len0=%d", len(a.Frames), len(a.Frames[0]))
	}
	if a.Frames[0][0] != 255 {
		t.Fatalf("f0[0]=%d", a.Frames[0][0])
	}
	if g := int(a.Frames[1][2*25+3]); g < 74 || g > 76 {
		t.Fatalf("green gray=%d", g)
	}
	if a.Delays[1] != 100*time.Millisecond {
		t.Fatalf("delay=%v", a.Delays[1])
	}
}

func TestLoadGIFDisposalAndSmallImage(t *testing.T) {
	pal := color.Palette{color.RGBA{0, 0, 0, 255}, color.RGBA{255, 255, 255, 255}}
	// 2x2 white patch at (1,1), disposed to background, then a black-only frame.
	f0 := image.NewPaletted(image.Rect(1, 1, 3, 3), pal)
	for i := range f0.Pix {
		f0.Pix[i] = 1
	}
	f1 := image.NewPaletted(image.Rect(0, 0, 1, 1), pal)
	p := writeGIF(t, &gif.GIF{Image: []*image.Paletted{f0, f1}, Delay: []int{0, 5},
		Disposal: []byte{gif.DisposalBackground, gif.DisposalNone}, Config: image.Config{Width: 3, Height: 3, ColorModel: pal}})
	a, err := LoadGIF(p)
	if err != nil {
		t.Fatal(err)
	}
	if a.Frames[0][1*25+1] != 255 || a.Frames[1][1*25+1] != 0 {
		t.Fatalf("disposal not applied: %d %d", a.Frames[0][26], a.Frames[1][26])
	}
	if a.Delays[0] != 100*time.Millisecond {
		t.Fatalf("zero delay -> %v", a.Delays[0])
	}
}

func TestAnimAtClampsToLast(t *testing.T) {
	a := &Anim{
		Frames: [][]uint8{{1}, {2}, {3}},
		Delays: []time.Duration{100 * time.Millisecond, 100 * time.Millisecond, 100 * time.Millisecond},
	}
	cases := []struct {
		d    time.Duration
		want uint8
	}{{0, 1}, {99 * time.Millisecond, 1}, {100 * time.Millisecond, 2}, {250 * time.Millisecond, 3}, {time.Hour, 3}}
	for _, c := range cases {
		if got := a.At(c.d)[0]; got != c.want {
			t.Errorf("At(%v)=%d want %d", c.d, got, c.want)
		}
	}
}

func TestLoadClockFontOrder(t *testing.T) {
	pal := color.Palette{color.RGBA{0, 0, 0, 255}, color.RGBA{255, 255, 255, 255}}
	g := &gif.GIF{}
	for i := 0; i < 11; i++ {
		im := image.NewPaletted(image.Rect(0, 0, 5, 9), pal)
		im.Pix[i] = 1
		g.Image = append(g.Image, im)
		g.Delay = append(g.Delay, 1)
		g.Disposal = append(g.Disposal, gif.DisposalBackground)
	}
	f, err := LoadClockFont(writeGIF(t, g))
	if err != nil {
		t.Fatal(err)
	}
	if !f.Colon[0] || !f.Digits[3][4] || f.Digits[3][3] {
		t.Fatalf("colon0=%v d3[4]=%v d3[3]=%v", f.Colon[0], f.Digits[3][4], f.Digits[3][3])
	}
}

func TestLoadClockFontTooFewFrames(t *testing.T) {
	pal := color.Palette{color.RGBA{0, 0, 0, 255}, color.RGBA{255, 255, 255, 255}}
	g := &gif.GIF{Image: []*image.Paletted{image.NewPaletted(image.Rect(0, 0, 5, 9), pal)}, Delay: []int{1}}
	if _, err := LoadClockFont(writeGIF(t, g)); err == nil {
		t.Fatal("want error")
	}
}

func TestBuiltinFontHasAllDigits(t *testing.T) {
	f := BuiltinFont()
	count := func(g [45]bool) (n int) {
		for _, b := range g {
			if b {
				n++
			}
		}
		return
	}
	for d, g := range f.Digits {
		if count(g) < 5 {
			t.Errorf("digit %d has %d pixels", d, count(g))
		}
	}
	if n := count(f.Colon); n != 2 {
		t.Errorf("colon has %d pixels", n)
	}
}

func TestParseLEDVolume(t *testing.T) {
	line := func(first, ms string) string {
		return first + strings.Repeat(" 000000", 23) + " " + ms + "\n"
	}
	in := "gammacorrection\nsmooth 80\n\n" + line("000500", "293") + line("001000", "100")
	p, err := ParseLED(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Frames) != 2 || p.Frames[0][0] != (RGB{0, 5, 0}) || p.Frames[1][0] != (RGB{0, 0x10, 0}) {
		t.Fatalf("frames=%v", p.Frames)
	}
	if p.Delays[0] != 293*time.Millisecond || p.Loop {
		t.Fatalf("delay=%v loop=%v", p.Delays[0], p.Loop)
	}
}

func TestParseLEDLoopAndComments(t *testing.T) {
	in := "# header\nloop\n  # indented\nff0080\t" + strings.Repeat("000000 ", 22) + "000000\t50\n"
	p, err := ParseLED(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Loop || len(p.Frames) != 1 || p.Frames[0][0] != (RGB{255, 0, 128}) || p.Delays[0] != 50*time.Millisecond {
		t.Fatalf("%+v", p)
	}
}

func TestParseLEDErrors(t *testing.T) {
	for _, in := range []string{"", "loop\n", "000000 10\n", "zz0000" + strings.Repeat(" 000000", 23) + " 10\n"} {
		if _, err := ParseLED(strings.NewReader(in)); err == nil {
			t.Errorf("want error for %q", in)
		}
	}
}

func TestLoadGIFMissingFile(t *testing.T) {
	if _, err := LoadGIF(filepath.Join(t.TempDir(), "nope.gif")); err == nil {
		t.Fatal("want error")
	}
}

func TestParseLEDSkipsGeneratorDirectives(t *testing.T) {
	in := "gradient 0 000000 1 000000 183\nrotate a707bb 9c08c6\n" + strings.Repeat("000000 ", 24) + "20\n"
	p, err := ParseLED(strings.NewReader(in))
	if err != nil || len(p.Frames) != 1 {
		t.Fatalf("err=%v p=%+v", err, p)
	}
}
