package frontend

import (
	"math"
	"math/rand"
	"testing"

	"voice/internal/capture"
)

const nch = capture.Channels

func rms(x []int16) float64 {
	var s float64
	for _, v := range x {
		s += float64(v) * float64(v)
	}
	return math.Sqrt(s / float64(max(1, len(x))))
}

func TestProcessLength(t *testing.T) {
	f := New(DefaultConfig())
	out := f.Process(make([]int16, capture.BlockFrames*nch))
	if len(out) != capture.BlockFrames/3 {
		t.Fatalf("len %d", len(out))
	}
}

func TestProcessSilentRefPassesVoice(t *testing.T) {
	f := New(DefaultConfig())
	block := make([]int16, capture.BlockFrames*nch)
	var all []int16
	n := 0
	for b := 0; b < 50; b++ {
		for i := 0; i < capture.BlockFrames; i++ {
			v := int16(1000 * math.Sin(2*math.Pi*1000*float64(n)/capture.Rate))
			n++
			for _, c := range []int{3, 4, 5} {
				block[i*nch+c] = v
			}
		}
		all = append(all, f.Process(block)...)
	}
	// Mic tone amplitude 1000 * gain 32 saturates; just require a strong signal.
	if r := rms(all[len(all)/2:]); r < 5000 {
		t.Fatalf("tone lost, rms %.0f", r)
	}
}

func TestProcessCancelsReference(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Gain = 1
	f := New(cfg)
	rng := rand.New(rand.NewSource(1))
	// Band-limited-ish music: smoothed noise.
	total := 6 * capture.Rate
	music := make([]float64, total)
	var lp float64
	for i := range music {
		lp += 0.3 * (rng.NormFloat64()*3000 - lp)
		music[i] = lp
	}
	const delay = 480 // 10 ms room delay
	block := make([]int16, capture.BlockFrames*nch)
	var in, out []int16
	for b := 0; b < total/capture.BlockFrames; b++ {
		for i := 0; i < capture.BlockFrames; i++ {
			n := b*capture.BlockFrames + i
			ref := int16(music[n])
			var mic int16
			if n >= delay {
				mic = int16(0.8 * music[n-delay])
			}
			for _, c := range []int{3, 4, 5} {
				block[i*nch+c] = mic
			}
			block[i*nch+11], block[i*nch+12] = ref, ref
		}
		o := f.Process(block)
		out = append(out, o...)
		for i := 0; i < capture.BlockFrames; i += 3 {
			in = append(in, block[i*nch+3])
		}
	}
	half := len(out) / 2
	ratio := 20 * math.Log10(rms(out[half:])/rms(in[half:]))
	t.Logf("residual %.1f dB", ratio)
	if ratio > -12 {
		t.Fatalf("only %.1f dB cancellation", -ratio)
	}
}
