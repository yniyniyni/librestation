package ycmd

import (
	"math"
	"math/cmplx"
)

// Kaldi's default fbank at 16 kHz: 25 ms povey window every 10 ms, DC
// removal, pre-emphasis 0.97, 512-point FFT, power spectrum, 40 mel bins
// over 20..8000 Hz, natural log, snip_edges. The model's flags ask for
// dither 0.1; we leave it out so the output is deterministic.
const (
	frameLen   = 400
	frameShift = 160
	nfft       = 512
	numMel     = 40
	preemph    = 0.97
)

// fbank turns int16-scale samples into log-mel frames. It holds the last
// frameLen samples; fill tops them up and frame computes one frame and
// slides the window by frameShift.
type fbank struct {
	win   [frameLen]float64
	melLo [numMel]int       // first FFT bin of each mel triangle
	melW  [numMel][]float64 // the triangle's weights from melLo on
	tw    [nfft / 2]complex128
	x     [frameLen]float64 // pending samples
	n     int               // how many of x are filled
	buf   [nfft]complex128
}

func melScale(f float64) float64 { return 1127 * math.Log(1+f/700) }

func (f *fbank) init() {
	for i := range f.win {
		f.win[i] = math.Pow(0.5-0.5*math.Cos(2*math.Pi*float64(i)/(frameLen-1)), 0.85)
	}
	for i := range f.tw {
		f.tw[i] = cmplx.Exp(complex(0, -2*math.Pi*float64(i)/nfft))
	}
	lo, hi := melScale(20), melScale(8000)
	delta := (hi - lo) / (numMel + 1)
	for b := range numMel {
		l, c, r := lo+float64(b)*delta, lo+float64(b+1)*delta, lo+float64(b+2)*delta
		f.melLo[b] = -1
		for k := range nfft / 2 { // Kaldi ignores the Nyquist bin
			m := melScale(16000 * float64(k) / nfft)
			if m <= l || m >= r {
				continue
			}
			if f.melLo[b] < 0 {
				f.melLo[b] = k
			}
			if m <= c {
				f.melW[b] = append(f.melW[b], (m-l)/(c-l))
			} else {
				f.melW[b] = append(f.melW[b], (r-m)/(r-c))
			}
		}
	}
}

func (f *fbank) reset() { f.n = 0 }

// fill takes samples from pcm until a frame is complete and reports how
// many it took and whether a frame is ready.
func (f *fbank) fill(pcm []float32) (int, bool) {
	k := min(len(pcm), frameLen-f.n)
	for i, v := range pcm[:k] {
		f.x[f.n+i] = float64(v)
	}
	f.n += k
	return k, f.n == frameLen
}

// frame writes the log-mel energies of the full window to out (numMel
// values) and drops the oldest frameShift samples.
func (f *fbank) frame(out []float32) {
	fr := f.x
	copy(f.x[:], f.x[frameShift:])
	f.n = frameLen - frameShift

	mean := 0.0
	for _, v := range fr {
		mean += v
	}
	mean /= frameLen
	for i := range fr {
		fr[i] -= mean
	}
	for i := frameLen - 1; i > 0; i-- {
		fr[i] -= preemph * fr[i-1]
	}
	fr[0] -= preemph * fr[0]
	for i := range f.buf {
		if i < frameLen {
			f.buf[i] = complex(fr[i]*f.win[i], 0)
		} else {
			f.buf[i] = 0
		}
	}
	f.fft()
	for b := range numMel {
		e := 0.0
		for j, w := range f.melW[b] {
			x := f.buf[f.melLo[b]+j]
			e += w * (real(x)*real(x) + imag(x)*imag(x))
		}
		out[b] = float32(math.Log(math.Max(e, 1.1920929e-07)))
	}
}

// fft is an in-place radix-2 transform of buf.
func (f *fbank) fft() {
	a := &f.buf
	for i, j := 1, 0; i < nfft; i++ {
		bit := nfft >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			a[i], a[j] = a[j], a[i]
		}
	}
	for size := 2; size <= nfft; size <<= 1 {
		half, step := size>>1, nfft/size
		for s := 0; s < nfft; s += size {
			for k := range half {
				t := f.tw[k*step] * a[s+k+half]
				a[s+k+half] = a[s+k] - t
				a[s+k] += t
			}
		}
	}
}
