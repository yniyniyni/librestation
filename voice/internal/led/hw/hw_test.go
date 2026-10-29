package hw

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"voice/internal/led/anim"
	"voice/internal/led/asset"
)

func tmpPaths(t *testing.T) Paths {
	t.Helper()
	d := t.TempDir()
	p := Paths{
		Screen:    filepath.Join(d, "fb4"),
		Ring:      filepath.Join(d, "fb5"),
		Backlight: filepath.Join(d, "brightness"),
		Lux:       filepath.Join(d, "lux"),
	}
	for _, f := range []string{p.Screen, p.Ring, p.Backlight} {
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func read(t *testing.T, f string) []byte {
	t.Helper()
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestDevicePaths(t *testing.T) {
	p := DevicePaths()
	if p.Screen != "/dev/graphics/fb4" || p.Ring != "/dev/graphics/fb5" ||
		p.Backlight != "/sys/class/backlight/fb4/brightness" ||
		p.Lux != "/sys/bus/iio/devices/iio:device0/in_intensity_input" {
		t.Fatalf("%+v", p)
	}
}

func TestDrawScreenGrayToRGB(t *testing.T) {
	p := tmpPaths(t)
	w, err := Open(p, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	var s anim.Screen
	s[0] = 128
	if err := w.Draw(s, anim.Ring{}, 1); err != nil {
		t.Fatal(err)
	}
	b := read(t, p.Screen)
	if len(b) != 1200 || b[0] != 128 || b[1] != 128 || b[2] != 128 || b[3] != 0 {
		t.Fatalf("len=%d % x", len(b), b[:6])
	}
}

func TestRingRemapAndBGR(t *testing.T) {
	p := tmpPaths(t)
	w, _ := Open(p, 5, false)
	defer w.Close()
	var r anim.Ring
	r[0] = asset.RGB{R: 1, G: 2, B: 3}
	r[1] = asset.RGB{R: 4, G: 5, B: 6}
	if err := w.Draw(anim.Screen{}, r, 1); err != nil {
		t.Fatal(err)
	}
	b := read(t, p.Ring)
	if len(b) != 72 {
		t.Fatalf("len %d", len(b))
	}
	if b[15] != 3 || b[16] != 2 || b[17] != 1 {
		t.Fatalf("hw5 % x", b[15:18])
	}
	if b[12] != 6 || b[13] != 5 || b[14] != 4 {
		t.Fatalf("hw4 % x", b[12:15])
	}
}

func TestRingIdentityClockwise(t *testing.T) {
	p := tmpPaths(t)
	w, _ := Open(p, 0, true)
	defer w.Close()
	var r anim.Ring
	r[7] = asset.RGB{R: 9}
	w.Draw(anim.Screen{}, r, 1)
	b := read(t, p.Ring)
	if b[21+2] != 9 {
		t.Fatalf("% x", b[21:24])
	}
}

func TestRingWrapNegative(t *testing.T) {
	p := tmpPaths(t)
	w, _ := Open(p, 0, false)
	defer w.Close()
	var r anim.Ring
	r[1] = asset.RGB{R: 9}
	w.Draw(anim.Screen{}, r, 1)
	b := read(t, p.Ring)
	if b[23*3+2] != 9 {
		t.Fatalf("% x", b[69:72])
	}
}

func TestDrawSkipsUnchanged(t *testing.T) {
	p := tmpPaths(t)
	w, _ := Open(p, 0, true)
	defer w.Close()
	var s anim.Screen
	s[3] = 50
	w.Draw(s, anim.Ring{}, 1)
	n := w.writes
	w.Draw(s, anim.Ring{}, 1)
	if w.writes != n {
		t.Fatalf("rewrote unchanged: %d -> %d", n, w.writes)
	}
	s[3] = 51
	w.Draw(s, anim.Ring{}, 1)
	if w.writes != n+1 {
		t.Fatalf("screen only should write once: %d", w.writes-n)
	}
	var r anim.Ring
	r[0].R = 1
	w.Draw(s, r, 1)
	if w.writes != n+2 {
		t.Fatalf("ring only should write once")
	}
}

func TestRingScale(t *testing.T) {
	p := tmpPaths(t)
	w, _ := Open(p, 0, true)
	defer w.Close()
	var r anim.Ring
	r[0] = asset.RGB{R: 0xFF, G: 0xFF, B: 0xFF}
	w.Draw(anim.Screen{}, r, 0.5)
	b := read(t, p.Ring)
	if b[0] != 0x7F && b[0] != 0x80 {
		t.Fatalf("% x", b[:3])
	}
}

func TestBrightness(t *testing.T) {
	for _, raw := range []float64{-1, 0, 1.5, 2, 1e9} {
		bl, sc := Brightness(raw)
		if bl < 1 || bl > 100 || sc < 0 || sc > 1 {
			t.Fatalf("raw %v -> %d %v", raw, bl, sc)
		}
	}
	if bl, sc := Brightness(2); bl != 3 || sc != 0.15 {
		t.Fatalf("dark %d %v", bl, sc)
	}
	if bl, sc := Brightness(-5); bl != 3 || sc != 0.15 {
		t.Fatalf("neg %d %v", bl, sc)
	}
	if bl, sc := Brightness(math.NaN()); bl != 3 || sc != 0.15 {
		t.Fatalf("nan %d %v", bl, sc)
	}
	if bl, sc := Brightness(200); bl != 100 || sc != 1.0 {
		t.Fatalf("bright %d %v", bl, sc)
	}
	if bl, sc := Brightness(1e9); bl != 100 || sc != 1.0 {
		t.Fatalf("huge %d %v", bl, sc)
	}
	pb, ps := 0, 0.0
	for raw := 0.0; raw < 400; raw += 0.5 {
		bl, sc := Brightness(raw)
		if bl < pb || sc < ps {
			t.Fatalf("not monotonic at %v", raw)
		}
		pb, ps = bl, sc
	}
	if bl, _ := Brightness(20); bl <= 3 || bl >= 100 {
		t.Fatalf("mid %d", bl)
	}
}

func TestSetBacklightOnlyOnChange(t *testing.T) {
	p := tmpPaths(t)
	w, _ := Open(p, 0, true)
	defer w.Close()
	if err := w.SetBacklight(36); err != nil {
		t.Fatal(err)
	}
	if string(read(t, p.Backlight)) != "36" {
		t.Fatalf("%q", read(t, p.Backlight))
	}
	n := w.writes
	w.SetBacklight(36)
	if w.writes != n {
		t.Fatal("rewrote")
	}
	w.SetBacklight(40)
	if string(read(t, p.Backlight)) != "40" {
		t.Fatalf("%q", read(t, p.Backlight))
	}
}

func TestReadLux(t *testing.T) {
	p := tmpPaths(t)
	os.WriteFile(p.Lux, []byte("1.532\n"), 0o644)
	w, _ := Open(p, 0, true)
	defer w.Close()
	v, err := w.ReadLux()
	if err != nil || v != 1.532 {
		t.Fatalf("%v %v", v, err)
	}
}
