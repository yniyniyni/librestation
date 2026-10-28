package dsp

// Deinterleave extracts the channels listed in chans from an interleaved
// int16 block with nch channels. Samples keep int16 scale (not normalised).
// The result never aliases block.
func Deinterleave(block []int16, nch int, chans []int) [][]float32 {
	frames := len(block) / nch
	out := make([][]float32, len(chans))
	for j, c := range chans {
		o := make([]float32, frames)
		for i := 0; i < frames; i++ {
			o[i] = float32(block[i*nch+c])
		}
		out[j] = o
	}
	return out
}

// MixMean returns the mean of the channels after subtracting each channel's
// block mean (stateless DC removal). Use Mixer for streaming.
func MixMean(chs [][]float32) []float32 {
	if len(chs) == 0 {
		return nil
	}
	out := make([]float32, len(chs[0]))
	for _, ch := range chs {
		var m float64
		for _, v := range ch {
			m += float64(v)
		}
		m /= float64(len(ch))
		for i, v := range ch {
			out[i] += float32(float64(v) - m)
		}
	}
	inv := 1 / float32(len(chs))
	for i := range out {
		out[i] *= inv
	}
	return out
}

const dcR = 0.999 // DC blocker pole (~7.6 Hz corner at 48 kHz)

// Mixer averages channels after a per-channel stateful DC blocker
// y[n] = x[n] - x[n-1] + R*y[n-1].
type Mixer struct {
	xp, yp []float64
}

func NewMixer(n int) *Mixer { return &Mixer{xp: make([]float64, n), yp: make([]float64, n)} }

// Mix returns a new slice with the DC-removed mean of chs (len(chs) <= n).
func (m *Mixer) Mix(chs [][]float32) []float32 {
	if len(chs) == 0 {
		return nil
	}
	out := make([]float32, len(chs[0]))
	for c, ch := range chs {
		xp, yp := m.xp[c], m.yp[c]
		for i, v := range ch {
			x := float64(v)
			y := x - xp + dcR*yp
			xp, yp = x, y
			out[i] += float32(y)
		}
		m.xp[c], m.yp[c] = xp, yp
	}
	inv := 1 / float32(len(chs))
	for i := range out {
		out[i] *= inv
	}
	return out
}
