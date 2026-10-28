// Package dsp holds small allocation-light DSP primitives: FFT, a 3:1
// decimator and a DC-removing channel mixer.
package dsp

import (
	"math"
	"sync"
)

type fftPlan struct {
	rev      []int
	cos, sin []float64 // twiddles e^{-2πik/n}, k < n/2
}

var plans sync.Map // int -> *fftPlan

func getPlan(n int) *fftPlan {
	if p, ok := plans.Load(n); ok {
		return p.(*fftPlan)
	}
	if n < 1 || n&(n-1) != 0 {
		panic("dsp: FFT length must be a power of two")
	}
	p := &fftPlan{rev: make([]int, n), cos: make([]float64, n/2), sin: make([]float64, n/2)}
	bits := 0
	for 1<<bits < n {
		bits++
	}
	for i := 0; i < n; i++ {
		r := 0
		for b := 0; b < bits; b++ {
			if i&(1<<b) != 0 {
				r |= 1 << (bits - 1 - b)
			}
		}
		p.rev[i] = r
	}
	for k := 0; k < n/2; k++ {
		a := -2 * math.Pi * float64(k) / float64(n)
		p.cos[k], p.sin[k] = math.Cos(a), math.Sin(a)
	}
	v, _ := plans.LoadOrStore(n, p)
	return v.(*fftPlan)
}

// FFT computes the in-place forward DFT. len(re)==len(im) must be a power of two.
func FFT(re, im []float64) { transform(re, im, false) }

// IFFT computes the in-place inverse DFT (scaled by 1/n).
func IFFT(re, im []float64) {
	transform(re, im, true)
	s := 1 / float64(len(re))
	for i := range re {
		re[i] *= s
		im[i] *= s
	}
}

func transform(re, im []float64, inv bool) {
	n := len(re)
	if len(im) != n {
		panic("dsp: FFT re/im length mismatch")
	}
	if n <= 1 {
		return
	}
	p := getPlan(n)
	for i, r := range p.rev {
		if i < r {
			re[i], re[r] = re[r], re[i]
			im[i], im[r] = im[r], im[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		half, step := size/2, n/size
		for start := 0; start < n; start += size {
			for k := 0; k < half; k++ {
				wr, wi := p.cos[k*step], p.sin[k*step]
				if inv {
					wi = -wi
				}
				a, b := start+k, start+k+half
				tr := re[b]*wr - im[b]*wi
				ti := re[b]*wi + im[b]*wr
				re[b], im[b] = re[a]-tr, im[a]-ti
				re[a] += tr
				im[a] += ti
			}
		}
	}
}
