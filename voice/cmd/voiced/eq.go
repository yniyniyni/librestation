package main

import (
	"math"
	"sync"
)

// eq is a 5-band parametric equalizer implemented as a chain of biquad IIR
// filters. It processes mono float32 samples in-place with no allocations.
//
// Each band is a peaking EQ filter (constant-Q) at a fixed center frequency;
// a preset sets only the gains. The filter runs at speakerRate (24 kHz).
type eq struct {
	mu      sync.Mutex
	preset  int // 0 = off
	bands   [eqBands]biquad
	enabled bool
}

const eqBands = 5

// The five center frequencies, in Hz. Chosen to span the speaker's useful
// range without overlapping too much at Q=1.4.
var eqFreqs = [eqBands]float64{60, 250, 1000, 4000, 12000}

// eqQ is the quality factor for all bands: 1.4 gives a moderate overlap.
const eqQ = 1.4

// eqPreset defines gain in dB for each of the 5 bands.
type eqPreset struct {
	Name string
	Gain [eqBands]float64 // dB, positive = boost
}

// Presets. Index 0 is "off" (flat). The order matches the UI.
var eqPresets = []eqPreset{
	{Name: "Обычный"},                                              // 0: flat / off
	{Name: "Бас", Gain: [eqBands]float64{8, 5, 0, -1, -2}},       // 1
	{Name: "Рэп", Gain: [eqBands]float64{6, 4, -1, 2, 1}},        // 2
	{Name: "Рок", Gain: [eqBands]float64{4, 1, -2, 3, 4}},        // 3
	{Name: "Поп", Gain: [eqBands]float64{-1, 2, 4, 3, 1}},        // 4
	{Name: "Джаз", Gain: [eqBands]float64{3, 0, -2, 2, 4}},       // 5
	{Name: "Голос", Gain: [eqBands]float64{-3, -1, 4, 5, 2}},     // 6
	{Name: "Классика", Gain: [eqBands]float64{0, 0, 0, -2, -3}},  // 7
	{Name: "Глубокий бас", Gain: [eqBands]float64{10, 6, -1, -2, -3}}, // 8
}

// EQPresets returns the preset list for the API/UI.
func EQPresets() []eqPreset { return eqPresets }

// SetPreset switches to preset p (0 = flat/off). Out of range → off.
func (e *eq) SetPreset(p int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if p < 0 || p >= len(eqPresets) {
		p = 0
	}
	e.preset = p
	if p == 0 {
		e.enabled = false
		for i := range e.bands {
			e.bands[i] = biquad{}
		}
		return
	}
	pr := eqPresets[p]
	for i := 0; i < eqBands; i++ {
		e.bands[i] = peakingEQ(eqFreqs[i], pr.Gain[i], eqQ, speakerRate)
	}
	e.enabled = true
}

// Preset returns the current preset index.
func (e *eq) Preset() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.preset
}

// Process applies the EQ to samples in-place. It is safe to call from one
// goroutine while another calls SetPreset.
func (e *eq) Process(samples []float32) {
	e.mu.Lock()
	if !e.enabled {
		e.mu.Unlock()
		return
	}
	// Copy the filter states locally so we release the lock fast; the biquad
	// states are small and there are only 5.
	var bands [eqBands]biquad
	copy(bands[:], e.bands[:])
	e.mu.Unlock()

	for i := range bands {
		bands[i].process(samples)
	}

	// Soft clip to prevent digital clipping from large boosts.
	for i, s := range samples {
		if s > 1 {
			samples[i] = 1 - 1/(s+1)
		} else if s < -1 {
			samples[i] = -1 + 1/(-s+1)
		}
	}

	e.mu.Lock()
	if e.enabled {
		copy(e.bands[:], bands[:])
	}
	e.mu.Unlock()
}

// biquad is a direct-form-II transposed biquad filter.
type biquad struct {
	b0, b1, b2 float64
	a1, a2     float64
	z1, z2     float64 // state
}

// peakingEQ computes coefficients for a peaking (parametric) EQ band.
//
//	f0   = center frequency (Hz)
//	dBgain = gain at f0 (positive = boost, negative = cut)
//	Q    = quality factor
//	fs   = sample rate (Hz)
func peakingEQ(f0, dBgain, Q, fs float64) biquad {
	if dBgain == 0 {
		return biquad{b0: 1}
	}
	A := math.Pow(10, dBgain/40)
	w0 := 2 * math.Pi * f0 / fs
	sin0, cos0 := math.Sincos(w0)
	alpha := sin0 / (2 * Q)
	a0 := 1 + alpha/A
	return biquad{
		b0: (1 + alpha*A) / a0,
		b1: (-2 * cos0) / a0,
		b2: (1 - alpha*A) / a0,
		a1: (-2 * cos0) / a0,
		a2: (1 - alpha/A) / a0,
	}
}

func (f *biquad) process(samples []float32) {
	if f.b0 == 0 {
		return
	}
	b0, b1, b2, a1, a2 := f.b0, f.b1, f.b2, f.a1, f.a2
	z1, z2 := f.z1, f.z2
	for i, s := range samples {
		x := float64(s)
		y := b0*x + z1
		z1 = b1*x - a1*y + z2
		z2 = b2*x - a2*y
		samples[i] = float32(y)
	}
	f.z1, f.z2 = z1, z2
}
