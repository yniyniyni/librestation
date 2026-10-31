// Package frontend turns 16-channel 48 kHz capture blocks into cleaned
// 16 kHz mono audio: mic mix, echo cancellation against the playback
// reference, and make-up gain.
package frontend

import (
	"voice/internal/aec"
	"voice/internal/capture"
	"voice/internal/dsp"
)

// Config selects channels and gain. Zero-valued fields take the defaults.
type Config struct {
	MicChannels []int
	RefChannels []int
	Gain        float64
}

// DefaultConfig is the Station Max layout: mics 3-5, playback L/R on 11-12.
func DefaultConfig() Config {
	return Config{MicChannels: []int{3, 4, 5}, RefChannels: []int{11, 12}, Gain: 32}
}

// Frontend is stateful; feed it consecutive blocks.
type Frontend struct {
	cfg      Config
	micMix   *dsp.Mixer
	refMix   *dsp.Mixer
	micDec   *dsp.Decimator3
	refDec   *dsp.Decimator3
	canceler *aec.Canceller
}

func New(cfg Config) *Frontend {
	d := DefaultConfig()
	if len(cfg.MicChannels) == 0 {
		cfg.MicChannels = d.MicChannels
	}
	if len(cfg.RefChannels) == 0 {
		cfg.RefChannels = d.RefChannels
	}
	if cfg.Gain == 0 {
		cfg.Gain = d.Gain
	}
	return &Frontend{
		cfg:      cfg,
		micMix:   dsp.NewMixer(capture.BlockFrames),
		refMix:   dsp.NewMixer(capture.BlockFrames),
		micDec:   dsp.NewDecimator3(),
		refDec:   dsp.NewDecimator3(),
		canceler: aec.New(64, 64),
	}
}

// Process consumes one block (BlockFrames*Channels interleaved int16) and
// returns BlockFrames/3 fresh 16 kHz samples. It does not retain block.
func (f *Frontend) Process(block []int16) []int16 {
	out, _ := f.ProcessRef(block)
	return out
}

// ProcessRef is Process that also returns the playback reference at 16 kHz
// (int16 scale, no gain): what the speaker itself played in this block.
func (f *Frontend) ProcessRef(block []int16) (out []int16, ref []float32) {
	mic := f.micDec.Process(f.micMix.Mix(dsp.Deinterleave(block, capture.Channels, f.cfg.MicChannels)))
	ref = f.refDec.Process(f.refMix.Mix(dsp.Deinterleave(block, capture.Channels, f.cfg.RefChannels)))
	clean := f.canceler.Process(mic, ref)
	out = make([]int16, len(clean))
	for i, v := range clean {
		out[i] = sat(float64(v) * f.cfg.Gain)
	}
	return out, ref
}

func sat(x float64) int16 {
	switch {
	case x > 32767:
		return 32767
	case x < -32768:
		return -32768
	}
	if x < 0 {
		return int16(x - 0.5)
	}
	return int16(x + 0.5)
}
