package ycmd

import (
	"math"
	"math/rand"
	"testing"

	"voice/internal/capture"
)

func TestInputChannel0(t *testing.T) {
	in := NewInput()
	rng := rand.New(rand.NewSource(1))
	const blocks = 20
	var got, want []float64
	for b := 0; b < blocks; b++ {
		blk := make([]int16, capture.BlockFrames*capture.Channels)
		for i := 0; i < capture.BlockFrames; i++ {
			n := b*capture.BlockFrames + i
			s := 8000 * math.Sin(2*math.Pi*300*float64(n)/capture.Rate)
			blk[i*capture.Channels] = int16(s)
			for c := 1; c < capture.Channels; c++ {
				blk[i*capture.Channels+c] = int16(rng.Intn(60000) - 30000)
			}
		}
		out := in.Process(blk)
		if len(out) != 320 {
			t.Fatalf("block %d: %d samples, want 320", b, len(out))
		}
		if b < 2 {
			continue // filter warm-up
		}
		for i, v := range out {
			got = append(got, float64(v))
			// 16 kHz sample i is 48 kHz frame 3i; the decimator delays by
			// a fixed amount, handled by the lag search below.
			want = append(want, 8000*math.Sin(2*math.Pi*300*float64(b*capture.BlockFrames+3*i)/capture.Rate)*InputGain)
		}
	}
	// Best correlation over a small lag window (filter group delay).
	var best, bestRatio float64
	for lag := 0; lag < 40; lag++ {
		var xy, xx, yy float64
		for i := lag; i < len(got); i++ {
			xy += got[i] * want[i-lag]
			xx += got[i] * got[i]
			yy += want[i-lag] * want[i-lag]
		}
		if c := xy / math.Sqrt(xx*yy); c > best {
			best, bestRatio = c, xy/yy
		}
	}
	if best < 0.99 {
		t.Errorf("correlation with channel-0 sine %.3f, want > 0.99 (noise leaked?)", best)
	}
	if math.Abs(bestRatio-1) > 0.05 {
		t.Errorf("amplitude ratio %.3f of expected (sine x InputGain), want 1", bestRatio)
	}
}
