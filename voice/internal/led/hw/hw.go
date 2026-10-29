// Package hw writes rendered frames to the Station Max framebuffers and sets
// screen/ring brightness from the light sensor.
package hw

import (
	"bytes"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"voice/internal/led/anim"
)

// Paths locates the devices ledd drives.
type Paths struct{ Screen, Ring, Backlight, Lux string }

// DevicePaths returns the paths measured on the Station Max.
func DevicePaths() Paths {
	return Paths{
		Screen:    "/dev/graphics/fb4",
		Ring:      "/dev/graphics/fb5",
		Backlight: "/sys/class/backlight/fb4/brightness",
		Lux:       "/sys/bus/iio/devices/iio:device0/in_intensity_input",
	}
}

const ringN = 24

// Writer owns the open framebuffers and the last frames written.
type Writer struct {
	p         Paths
	screen    *os.File
	ring      *os.File
	ringTop   int
	clockwise bool

	lastScreen []byte
	lastRing   []byte
	backlight  int // last value written; 0 = none yet
	writes     int // number of device writes, for tests

	now      func() time.Time // injectable clock
	screenAt time.Time        // last screen write
	ringAt   time.Time        // last ring write
}

// The ring driver can drop a frame written right after a fast stream, so an
// unchanged frame is rewritten once the last write is this old.
const (
	ringRefresh   = 250 * time.Millisecond
	screenRefresh = time.Second
)

// Open opens both framebuffers. Logical ring index i maps to hardware index
// (ringTop+i) mod 24 when clockwise, (ringTop-i) mod 24 otherwise.
func Open(p Paths, ringTop int, clockwise bool) (*Writer, error) {
	s, err := os.OpenFile(p.Screen, os.O_WRONLY, 0)
	if err != nil {
		return nil, err
	}
	r, err := os.OpenFile(p.Ring, os.O_WRONLY, 0)
	if err != nil {
		s.Close()
		return nil, err
	}
	return &Writer{p: p, screen: s, ring: r, ringTop: ringTop, clockwise: clockwise, now: time.Now}, nil
}

// Close closes the framebuffers.
func (w *Writer) Close() error {
	e1 := w.screen.Close()
	e2 := w.ring.Close()
	if e1 != nil {
		return e1
	}
	return e2
}

func (w *Writer) hwIndex(i int) int {
	if w.clockwise {
		return ((w.ringTop+i)%ringN + ringN) % ringN
	}
	return ((w.ringTop-i)%ringN + ringN) % ringN
}

func scale(c uint8, f float64) byte {
	v := math.Round(float64(c) * f)
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

// Draw writes the screen (gray to RGB) and ring (remapped, BGR, scaled by
// ringScale) frames, skipping each device when its frame is unchanged and was written
// recently (see ringRefresh, screenRefresh).
func (w *Writer) Draw(s anim.Screen, r anim.Ring, ringScale float64) error {
	sb := make([]byte, 0, len(s)*3)
	for _, g := range s {
		sb = append(sb, g, g, g)
	}
	rb := make([]byte, ringN*3)
	for i, c := range r {
		o := w.hwIndex(i) * 3
		rb[o], rb[o+1], rb[o+2] = scale(c.B, ringScale), scale(c.G, ringScale), scale(c.R, ringScale)
	}
	now := w.now()
	if !bytes.Equal(sb, w.lastScreen) || now.Sub(w.screenAt) >= screenRefresh {
		if _, err := w.screen.WriteAt(sb, 0); err != nil {
			return err
		}
		w.lastScreen, w.screenAt = sb, now
		w.writes++
	}
	if !bytes.Equal(rb, w.lastRing) || now.Sub(w.ringAt) >= ringRefresh {
		if _, err := w.ring.WriteAt(rb, 0); err != nil {
			return err
		}
		w.lastRing, w.ringAt = rb, now
		w.writes++
	}
	return nil
}

// Brightness maps a lux reading to a backlight level (1..100) and a ring
// scale (0..1). Night-only calibration: lux <= 2 -> 3 / 0.15, lux >= 200 ->
// 100 / 1.0, log10-interpolated between. NaN or negative lux counts as dark.
func Brightness(lux float64) (backlight int, ringScale float64) {
	const (
		loLux, hiLux = 2.0, 200.0
		loBL, hiBL   = 3.0, 100.0
		loRS, hiRS   = 0.15, 1.0
	)
	var t float64
	switch {
	case math.IsNaN(lux) || lux <= loLux:
		t = 0
	case lux >= hiLux:
		t = 1
	default:
		t = (math.Log10(lux) - math.Log10(loLux)) / (math.Log10(hiLux) - math.Log10(loLux))
	}
	backlight = int(math.Round(loBL + t*(hiBL-loBL)))
	if backlight < 1 {
		backlight = 1
	}
	if backlight > 100 {
		backlight = 100
	}
	return backlight, loRS + t*(hiRS-loRS)
}

// SetBacklightPath writes a backlight level (clamped to 1..100) to path
// without needing open framebuffers.
func SetBacklightPath(path string, v int) error {
	v = min(max(v, 1), 100)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString(strconv.Itoa(v))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// SetBacklight writes the backlight level, only when it changed.
func (w *Writer) SetBacklight(v int) error {
	v = min(max(v, 1), 100)
	if v == w.backlight {
		return nil
	}
	if err := SetBacklightPath(w.p.Backlight, v); err != nil {
		return err
	}
	w.backlight = v
	w.writes++
	return nil
}

// ReadLux reads the light sensor.
func (w *Writer) ReadLux() (float64, error) {
	b, err := os.ReadFile(w.p.Lux)
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
}
