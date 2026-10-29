package dsp

import (
	"math"
	"testing"
)

func TestBands1kHzSine(t *testing.T) {
	b := NewBands(48000)
	x := make([]float64, 4800)
	for i := range x {
		x[i] = math.Sin(2 * math.Pi * 1000 * float64(i) / 48000)
	}
	b.Push(x)
	l := b.Levels()
	best := 0
	for i, v := range l {
		if v > l[best] {
			best = i
		}
	}
	lo := 60 * math.Pow(16000.0/60, float64(best)/12)
	hi := 60 * math.Pow(16000.0/60, float64(best+1)/12)
	if lo > 1000 || hi < 1000 {
		t.Fatalf("max band %d covers %.0f..%.0f Hz, not 1 kHz (levels %v)", best, lo, hi, l)
	}
	if l[best] < 0.9 {
		t.Fatalf("peak level %.3f < 0.9", l[best])
	}
	if l[0] > 0.1 {
		t.Fatalf("band 0 = %.3f, want <= 0.1", l[0])
	}
}

func TestBandsSilence(t *testing.T) {
	b := NewBands(48000)
	b.Push(make([]float64, 2000))
	for i, v := range b.Levels() {
		if v > 0.01 {
			t.Fatalf("band %d = %.3f on silence", i, v)
		}
	}
}

func sineBlock(amp, hz float64, n int) []float64 {
	x := make([]float64, n)
	for i := range x {
		x[i] = amp * math.Sin(2*math.Pi*hz*float64(i)/48000)
	}
	return x
}

// Post-amplifier reference at normal volume is far below full scale: the
// auto-gain must still lift a -40 dBFS tone to the top of its band.
func TestBandsAutoGainQuietSine(t *testing.T) {
	b := NewBands(48000)
	var l [bandsCount]float64
	for i := 0; i < 10; i++ {
		b.Push(sineBlock(0.01, 1000, 1920))
		l = b.Levels()
	}
	best := 0
	for i, v := range l {
		if v > l[best] {
			best = i
		}
	}
	if l[best] < 0.9 {
		t.Fatalf("quiet sine peak %.3f < 0.9 (levels %v)", l[best], l)
	}
}

func TestBandsAutoGainSilenceAfterLoud(t *testing.T) {
	b := NewBands(48000)
	b.Push(sineBlock(0.0003, 3000, 1024)) // -70 dBFS
	for i := 0; i < 5; i++ {
		b.Levels()
	}
	b.Push(make([]float64, 1024))
	for i := 0; i < 10; i++ {
		b.Levels()
	}
	for i, v := range b.Levels() {
		if v > 0.01 {
			t.Fatalf("band %d = %.3f on silence", i, v)
		}
	}
}
