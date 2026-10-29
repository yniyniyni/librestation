package dsp

import "math"

const decTaps = 96

var decCoef = designLowpass(decTaps, 7000.0/48000.0, 7.0)

// designLowpass builds a Kaiser-windowed sinc low-pass (fc in cycles/sample), unity DC gain.
func designLowpass(n int, fc, beta float64) []float32 {
	h := make([]float64, n)
	m := float64(n-1) / 2
	var sum float64
	for i := range h {
		x := float64(i) - m
		s := 2 * fc
		if x != 0 {
			s = math.Sin(2*math.Pi*fc*x) / (math.Pi * x)
		}
		r := x / m
		w := bessel0(beta*math.Sqrt(1-r*r)) / bessel0(beta)
		h[i] = s * w
		sum += h[i]
	}
	out := make([]float32, n)
	for i := range h {
		out[i] = float32(h[i] / sum)
	}
	return out
}

func bessel0(x float64) float64 {
	sum, term := 1.0, 1.0
	for k := 1; k < 50; k++ {
		term *= (x / (2 * float64(k))) * (x / (2 * float64(k)))
		sum += term
	}
	return sum
}

// Decimator3 converts 48 kHz to 16 kHz with a 96-tap FIR low-pass (~7 kHz
// cutoff). It is stateful: feeding data in any chunking yields the same output
// stream. Output is delayed by (taps-1)/2 input samples.
type Decimator3 struct {
	buf []float32 // history (taps-1 zeros initially) followed by unconsumed input
}

func NewDecimator3() *Decimator3 {
	return &Decimator3{buf: make([]float32, decTaps-1, decTaps-1+2048)}
}

// Process consumes in and returns the newly available 16 kHz samples in a
// freshly allocated slice.
func (d *Decimator3) Process(in []float32) []float32 {
	d.buf = append(d.buf, in...)
	nout := 0
	if len(d.buf) >= decTaps {
		nout = (len(d.buf)-decTaps)/3 + 1
	}
	out := make([]float32, nout)
	for k := range out {
		seg := d.buf[3*k : 3*k+decTaps]
		var acc float32
		for i, c := range decCoef {
			acc += c * seg[i]
		}
		out[k] = acc
	}
	// keep unconsumed tail: samples from index 3*nout onward
	keep := d.buf[3*nout:]
	copy(d.buf, keep)
	d.buf = d.buf[:len(keep)]
	return out
}
