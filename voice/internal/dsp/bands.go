package dsp

import (
	"math"
	"sync"
)

const (
	bandsN     = 1024
	bandsCount = 12
	bandsLoHz  = 60.0
	bandsHiHz  = 16000.0
	bandsFloor = -60.0 // dBFS mapped to 0
)

// Bands is a small spectrum analyser: it keeps the latest 1024 samples and,
// on Levels, returns 12 log-spaced band levels between 60 Hz and 16 kHz.
// Push is cheap; the FFT runs only in Levels. Safe for concurrent use.
type Bands struct {
	mu     sync.Mutex
	win    [bandsN]float64
	ring   [bandsN]float64
	pos    int // next write index
	lo, hi [bandsCount]int
	norm   float64
	re, im []float64
}

// NewBands makes an analyser for samples at the given rate.
func NewBands(rate int) *Bands {
	b := &Bands{re: make([]float64, bandsN), im: make([]float64, bandsN)}
	var sum float64
	for i := range b.win {
		b.win[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/bandsN)
		sum += b.win[i]
	}
	b.norm = 2 / sum // a full-scale sine peaks at magnitude 1
	df := float64(rate) / bandsN
	edge := func(i int) int {
		f := bandsLoHz * math.Pow(bandsHiHz/bandsLoHz, float64(i)/bandsCount)
		return int(f/df + 0.5)
	}
	for i := 0; i < bandsCount; i++ {
		lo, hi := edge(i), edge(i+1)
		lo = min(max(lo, 1), bandsN/2-1)
		hi = min(max(hi, lo+1), bandsN/2)
		b.lo[i], b.hi[i] = lo, hi
	}
	return b
}

// Push appends samples scaled to ±1.
func (b *Bands) Push(x []float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, v := range x {
		b.ring[b.pos] = v
		b.pos = (b.pos + 1) % bandsN
	}
}

// Levels returns each band's peak level, dBFS −60..0 mapped to 0..1.
func (b *Bands) Levels() [bandsCount]float64 {
	b.mu.Lock()
	for i := 0; i < bandsN; i++ {
		b.re[i] = b.ring[(b.pos+i)%bandsN] * b.win[i]
		b.im[i] = 0
	}
	b.mu.Unlock()
	FFT(b.re, b.im)
	var out [bandsCount]float64
	for i := range out {
		var m float64
		for k := b.lo[i]; k < b.hi[i]; k++ {
			m = max(m, math.Hypot(b.re[k], b.im[k]))
		}
		db := 20 * math.Log10(max(m*b.norm, 1e-9))
		out[i] = min(max((db-bandsFloor)/-bandsFloor, 0), 1)
	}
	return out
}
