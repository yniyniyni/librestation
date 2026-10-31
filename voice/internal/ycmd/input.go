package ycmd

import (
	"voice/internal/capture"
	"voice/internal/dsp"
)

// InputGain scales the 16 kHz channel-0 signal (int16 units) to what the
// Yandex spotter expects.
const InputGain = 1.0 / 64

// Input turns raw capture blocks into the spotter's 16 kHz float stream:
// channel 0, DC-removing mixer, 3:1 decimator, InputGain.
type Input struct {
	ch  []float32
	mix *dsp.Mixer
	dec *dsp.Decimator3
}

func NewInput() *Input {
	return &Input{
		ch:  make([]float32, capture.BlockFrames),
		mix: dsp.NewMixer(capture.BlockFrames),
		dec: dsp.NewDecimator3(),
	}
}

// Process takes one block of capture.BlockFrames*capture.Channels
// interleaved samples and returns capture.BlockFrames/3 samples at 16 kHz.
// The result is only valid until the next call.
func (in *Input) Process(block []int16) []float32 {
	for i := range in.ch {
		in.ch[i] = float32(block[i*capture.Channels])
	}
	out := in.dec.Process(in.mix.Mix([][]float32{in.ch}))
	for i := range out {
		out[i] *= InputGain
	}
	return out
}
