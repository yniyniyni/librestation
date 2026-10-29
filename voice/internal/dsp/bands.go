package dsp

import (
	"math"
	"sync"
)

const (
	bandsN         = 1024
	bandsCount     = 12
	bandsLoHz      = 60.0
	bandsHiHz      = 16000.0
	bandsRange     = 40.0  // dB window below the running peak mapped to 0..1
	bandsTilt      = 2.0   // dB added per band index: highs carry less energy
	bandsDecay     = 0.25  // dB the running peak falls per Levels call (~6 dB/s at 25 calls/s)
	bandsPeakFloor = -85.0 // running peak never drops below this, so silence stays dark
)

// Bands is a small spectrum analyser: it keeps the latest 1024 samples and,
// on Levels, returns 12 log-spaced band levels between 60 Hz and 16 kHz.
// Push is cheap; the FFT runs only in Levels. Push may run concurrently with
// Levels, but Levels itself keeps auto-gain state and is for one caller.
type Bands struct {
	mu     sync.Mutex
	win    [bandsN]float64
	ring   [bandsN]float64
	pos    int // next write index
	lo, hi [bandsCount]int
	norm   float64
	re, im []float64
	peak   float64 // running peak in dBFS for auto-gain (Levels caller only)
}

// NewBands makes an analyser for samples at the given rate.
func NewBands(rate int) *Bands {
	b := &Bands{re: make([]float64, bandsN), im: make([]float64, bandsN), peak: bandsPeakFloor}
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

// Levels returns each band's peak level as 0..1. The playback reference is
// post-amplifier, so its absolute level follows the volume; levels are
// relative to a running peak (auto-gain) over a 40 dB window, with a small
// per-band tilt. Stateful: call it from one goroutine.
func (b *Bands) Levels() [bandsCount]float64 {
	b.mu.Lock()
	for i := 0; i < bandsN; i++ {
		b.re[i] = b.ring[(b.pos+i)%bandsN] * b.win[i]
		b.im[i] = 0
	}
	b.mu.Unlock()
	FFT(b.re, b.im)
	var dbs [bandsCount]float64
	maxDB := math.Inf(-1)
	for i := range dbs {
		var m float64
		for k := b.lo[i]; k < b.hi[i]; k++ {
			m = max(m, math.Hypot(b.re[k], b.im[k]))
		}
		dbs[i] = 20 * math.Log10(max(m*b.norm, 1e-9))
		maxDB = max(maxDB, dbs[i])
	}
	b.peak = max(maxDB, b.peak-bandsDecay, bandsPeakFloor)
	bottom := b.peak - bandsRange
	var out [bandsCount]float64
	for i := range out {
		out[i] = min(max((dbs[i]+bandsTilt*float64(i)-bottom)/bandsRange, 0), 1)
	}
	return out
}
