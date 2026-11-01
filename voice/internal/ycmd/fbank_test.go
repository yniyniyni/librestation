package ycmd

import (
	"math"
	"math/cmplx"
	"testing"
)

func sine(amp, hz float64, n int) []float32 {
	x := make([]float32, n)
	for i := range x {
		x[i] = float32(amp * math.Sin(2*math.Pi*hz*float64(i)/16000))
	}
	return x
}

// allFrames runs pcm through a fresh fbank and returns every frame.
func allFrames(pcm []float32) [][numMel]float32 {
	var f fbank
	f.init()
	var out [][numMel]float32
	for len(pcm) > 0 {
		n, ok := f.fill(pcm)
		pcm = pcm[n:]
		if ok {
			var o [numMel]float32
			f.frame(o[:])
			out = append(out, o)
		}
	}
	return out
}

// kaldiFrame computes one fbank frame the slow way, straight from Kaldi's
// definition: DC removal, pre-emphasis 0.97, povey window, 512-point DFT,
// power spectrum, 40 triangular mel bins over 20..8000 Hz, natural log.
func kaldiFrame(s []float32) [numMel]float64 {
	x := make([]float64, 400)
	mean := 0.0
	for i := range x {
		x[i] = float64(s[i])
		mean += x[i] / 400
	}
	for i := range x {
		x[i] -= mean
	}
	for i := 399; i > 0; i-- {
		x[i] -= 0.97 * x[i-1]
	}
	x[0] -= 0.97 * x[0]
	for i := range x {
		x[i] *= math.Pow(0.5-0.5*math.Cos(2*math.Pi*float64(i)/399), 0.85)
	}
	power := make([]float64, 256)
	for k := range power {
		var v complex128
		for i, xi := range x {
			v += complex(xi, 0) * cmplx.Exp(complex(0, -2*math.Pi*float64(k*i)/512))
		}
		power[k] = real(v)*real(v) + imag(v)*imag(v)
	}
	mel := func(f float64) float64 { return 1127 * math.Log(1+f/700) }
	lo, hi := mel(20), mel(8000)
	d := (hi - lo) / 41
	var out [numMel]float64
	for b := range out {
		l, c, r := lo+float64(b)*d, lo+float64(b+1)*d, lo+float64(b+2)*d
		e := 0.0
		for k, p := range power {
			m := mel(float64(k) * 16000 / 512)
			switch {
			case m > l && m <= c:
				e += p * (m - l) / (c - l)
			case m > c && m < r:
				e += p * (r - m) / (r - c)
			}
		}
		out[b] = math.Log(max(e, 1.1920929e-07))
	}
	return out
}

func TestFbankKaldi(t *testing.T) {
	pcm := sine(1000, 1000, 16000)
	frames := allFrames(pcm)
	if len(frames) != 98 {
		t.Fatalf("%d frames, want 98", len(frames))
	}
	// The band whose triangle weighs 1 kHz most.
	mel := func(f float64) float64 { return 1127 * math.Log(1+f/700) }
	lo, d := mel(20), (mel(8000)-mel(20))/41
	band := int(math.Round((mel(1000)-lo)/d)) - 1
	for i, fr := range frames {
		best := 0
		for b := range fr {
			if fr[b] > fr[best] {
				best = b
			}
		}
		if best != band {
			t.Fatalf("frame %d: loudest band %d, want %d (1 kHz)", i, best, band)
		}
	}
	for _, i := range []int{0, 37} {
		want := kaldiFrame(pcm[160*i:])
		for b, w := range want {
			if got := float64(frames[i][b]); math.Abs(got-w) > 1e-3*max(1, math.Abs(w)) {
				t.Errorf("frame %d band %d = %.5f, want %.5f", i, b, got, w)
			}
		}
	}
}
