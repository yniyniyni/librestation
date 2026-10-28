// Package aec is a partitioned-block frequency-domain NLMS acoustic echo
// canceller (MDF style, overlap-save) for 16 kHz mono audio.
//
// Samples are float32 in int16 scale. The adaptation step is normalised per
// bin by the reference power held in the filter, so the canceller behaves the
// same for loud and very quiet signals. With a silent reference the filter
// output is zero and the mic passes through unchanged.
package aec

import (
	"math"

	"voice/internal/dsp"
)

const (
	mu = 0.5 // NLMS step size

	// refFloor is the per-sample reference power (int16 units²) below which
	// a block carries no usable reference: ~0.1 LSB RMS.
	refFloor = 0.01

	// Constrain this many partitions per block (round-robin, as in Speex's
	// AUMDF); the rest are updated unconstrained. Each partition is then
	// constrained every partitions/constrainPerBlock blocks.
	constrainPerBlock = 4

	fastAlpha = 0.5  // per-block smoothing of y/e energies (double-talk)
	slowAlpha = 0.05 // per-block smoothing for the convergence estimate

	// Double talk: instantaneous mic/output ratio dtDrop below the
	// long-term ERLE while the filter is known to be converged.
	dtDrop = 4.0 // 6 dB
	tiny   = 1e-30
	// convergedERLE: smoothed mic/output energy ratio above which the
	// filter counts as converged (≈3 dB).
	convergedERLE = 2.0
	dtHangover    = 10  // blocks adaptation stays frozen after double talk
	dtMaxBlocks   = 500 // ~2 s at block 64: assume echo-path change, re-learn
	// divergeRatio: output energy this far above mic energy for longer
	// than the filter span means the filter has diverged; reset it. (A
	// single block can legitimately exceed it, e.g. when music resumes and
	// the echo has not reached the mic yet.)
	divergeRatio = 8.0
)

// Canceller removes the echo of ref from mic. Not safe for concurrent use.
type Canceller struct {
	n, m, nb, p int // block, FFT size, bins (n+1), partitions

	xRe, xIm []float64 // reference spectra ring, p×nb
	wRe, wIm []float64 // filter weights, p×nb
	xPow     []float64 // Σ_p |X_p|² per bin
	head     int       // ring slot of the newest reference spectrum
	nextCons int       // next partition to constrain
	blocks   int       // blocks since power sum was last recomputed

	xTail    []float64 // previous reference block
	re, im   []float64 // FFT scratch, m
	eRe, eIm []float64 // error spectrum, nb
	y        []float64 // echo estimate for the current block

	emFast, eeFast      float64
	emSlow, eeSlow      float64
	hang, dtRun, divRun int

	out []float32
}

// New returns a canceller with the given block size (power of two) and
// number of partitions; the filter spans block×partitions taps.
func New(block, partitions int) *Canceller {
	if block < 1 || block&(block-1) != 0 || partitions < 1 {
		panic("aec: block must be a power of two and partitions >= 1")
	}
	n, p := block, partitions
	nb := n + 1
	c := &Canceller{
		n: n, m: 2 * n, nb: nb, p: p,
		xRe: make([]float64, p*nb), xIm: make([]float64, p*nb),
		wRe: make([]float64, p*nb), wIm: make([]float64, p*nb),
		xPow:  make([]float64, nb),
		xTail: make([]float64, n),
		re:    make([]float64, 2*n), im: make([]float64, 2*n),
		eRe: make([]float64, nb), eIm: make([]float64, nb),
		y: make([]float64, n),
	}
	return c
}

// Process cancels echo in mic given the time-aligned playback reference ref.
// len(mic) must equal len(ref) and be a multiple of the block size. The
// returned slice is owned by the Canceller and valid until the next call.
func (c *Canceller) Process(mic, ref []float32) []float32 {
	if len(mic) != len(ref) || len(mic)%c.n != 0 {
		panic("aec: mic/ref length mismatch or not a multiple of block")
	}
	if cap(c.out) < len(mic) {
		c.out = make([]float32, len(mic))
	}
	out := c.out[:len(mic)]
	for off := 0; off < len(mic); off += c.n {
		c.block(mic[off:off+c.n], ref[off:off+c.n], out[off:off+c.n])
	}
	return out
}

func (c *Canceller) block(mic, ref []float32, out []float32) {
	n, nb, p := c.n, c.nb, c.p
	re, im := c.re, c.im

	// 1. Reference spectrum of [previous block, current block].
	var refE float64
	copy(re, c.xTail)
	for i, v := range ref {
		f := float64(v)
		re[n+i] = f
		c.xTail[i] = f
		refE += f * f
	}
	clear(im)
	dsp.FFT(re, im)
	c.head = (c.head + 1) % p
	slot := c.head * nb
	for k := 0; k < nb; k++ {
		c.xPow[k] -= c.xRe[slot+k]*c.xRe[slot+k] + c.xIm[slot+k]*c.xIm[slot+k]
		c.xRe[slot+k], c.xIm[slot+k] = re[k], im[k]
		c.xPow[k] += re[k]*re[k] + im[k]*im[k]
	}
	if c.blocks++; c.blocks >= p { // kill float drift in the running sum
		c.blocks = 0
		c.recomputePower()
	}

	// 2. Echo estimate Y = Σ_p W_p · X_{head-p}.
	c.filter()
	copy(c.y, re[n:])

	// 3. Error (= output) and energies.
	var ee, em float64
	for i := range mic {
		m := float64(mic[i])
		e := m - c.y[i]
		out[i] = float32(e)
		ee += e * e
		em += m * m
	}
	if ee > divergeRatio*em {
		c.divRun++
	} else {
		c.divRun = 0
	}
	if math.IsNaN(ee) || math.IsInf(ee, 0) || c.divRun > c.p {
		// Diverged (output well above mic for a whole filter length):
		// drop the filter, pass the mic through.
		c.reset()
		copy(out, mic)
		return
	}

	// 4. Adapt unless there is no reference or we are in double talk.
	if refE < refFloor*float64(n) || !c.shouldAdapt(ee, em) {
		return
	}
	clear(re[:n])
	for i := range out {
		re[n+i] = float64(out[i])
	}
	clear(im)
	dsp.FFT(re, im)
	copy(c.eRe, re[:nb])
	copy(c.eIm, im[:nb])
	c.adapt()
	for j := 0; j < constrainPerBlock && j < p; j++ {
		c.constrain(c.nextCons)
		c.nextCons = (c.nextCons + 1) % p
	}
}

// shouldAdapt runs the double-talk guard on block energies of the output
// (ee) and mic (em). Once the filter has converged, a sudden drop of the
// instantaneous mic/output ratio well below its long-term value (the ERLE
// achieved so far) means something other than echo — near-end speech — is in
// the mic, and adaptation is frozen.
func (c *Canceller) shouldAdapt(ee, em float64) bool {
	c.emFast += fastAlpha * (em - c.emFast)
	c.eeFast += fastAlpha * (ee - c.eeFast)
	erle := c.emSlow / (c.eeSlow + tiny)
	cur := c.emFast / (math.Max(ee, c.eeFast) + tiny)
	dt := erle > convergedERLE && cur*dtDrop < erle
	if dt {
		c.hang = dtHangover
		c.dtRun++
		if c.dtRun > dtMaxBlocks {
			// Too long for speech: more likely the echo path changed.
			// Forget convergence so the filter re-learns.
			c.emSlow, c.eeSlow, c.dtRun, c.hang = 0, 0, 0, 0
		}
		return false
	}
	c.dtRun = 0
	if c.hang > 0 {
		c.hang--
		return false
	}
	c.emSlow += slowAlpha * (em - c.emSlow)
	c.eeSlow += slowAlpha * (ee - c.eeSlow)
	return true
}

// filter computes the overlap-save echo estimate into c.re[n:] (time domain).
func (c *Canceller) filter() {
	nb, p := c.nb, c.p
	re, im := c.re, c.im
	clear(re[:nb])
	clear(im[:nb])
	for j := 0; j < p; j++ {
		xo := ((c.head - j + p) % p) * nb
		wo := j * nb
		xr, xi := c.xRe[xo:xo+nb], c.xIm[xo:xo+nb]
		wr, wi := c.wRe[wo:wo+nb], c.wIm[wo:wo+nb]
		for k := range nb {
			re[k] += wr[k]*xr[k] - wi[k]*xi[k]
			im[k] += wr[k]*xi[k] + wi[k]*xr[k]
		}
	}
	c.inverse()
}

// adapt applies W_p += μ · conj(X_p) · E / (Σ|X|² + δ), unconstrained.
func (c *Canceller) adapt() {
	nb, p := c.nb, c.p
	// Regulariser: relative to the strongest bin's reference power so
	// weak bins in a strong reference are not over-amplified, plus an
	// absolute floor (~0.1 LSB) so near-silent references stay bounded.
	var maxPow float64
	for _, v := range c.xPow {
		maxPow = math.Max(maxPow, v)
	}
	delta := 1e-4*maxPow + refFloor*float64(c.m*p)
	gr, gi := c.eRe, c.eIm // scale E by μ/(Σ|X|²+δ) in place: reused for all p
	for k := range nb {
		s := mu / (c.xPow[k] + delta)
		gr[k] *= s
		gi[k] *= s
	}
	for j := 0; j < p; j++ {
		xo := ((c.head - j + p) % p) * nb
		wo := j * nb
		xr, xi := c.xRe[xo:xo+nb], c.xIm[xo:xo+nb]
		wr, wi := c.wRe[wo:wo+nb], c.wIm[wo:wo+nb]
		for k := range nb {
			// conj(X)·G
			wr[k] += xr[k]*gr[k] + xi[k]*gi[k]
			wi[k] += xr[k]*gi[k] - xi[k]*gr[k]
		}
	}
}

// constrain forces partition j's impulse response to the first n taps
// (zeroes the wrap-around half), keeping the overlap-save model linear.
func (c *Canceller) constrain(j int) {
	n, nb := c.n, c.nb
	wo := j * nb
	copy(c.re[:nb], c.wRe[wo:wo+nb])
	copy(c.im[:nb], c.wIm[wo:wo+nb])
	c.inverse()
	clear(c.re[n:])
	clear(c.im)
	dsp.FFT(c.re, c.im)
	copy(c.wRe[wo:wo+nb], c.re[:nb])
	copy(c.wIm[wo:wo+nb], c.im[:nb])
}

// inverse expands the half spectrum in c.re/c.im[:nb] by Hermitian symmetry
// and transforms it to the (real) time domain in c.re.
func (c *Canceller) inverse() {
	n, m := c.n, c.m
	re, im := c.re, c.im
	im[0], im[n] = 0, 0
	for k := 1; k < n; k++ {
		re[m-k], im[m-k] = re[k], -im[k]
	}
	dsp.IFFT(re, im)
}

func (c *Canceller) recomputePower() {
	clear(c.xPow)
	for j := 0; j < c.p; j++ {
		o := j * c.nb
		for k := range c.nb {
			c.xPow[k] += c.xRe[o+k]*c.xRe[o+k] + c.xIm[o+k]*c.xIm[o+k]
		}
	}
}

func (c *Canceller) reset() {
	clear(c.wRe)
	clear(c.wIm)
	c.emFast, c.eeFast, c.emSlow, c.eeSlow = 0, 0, 0, 0
	c.hang, c.dtRun, c.divRun = 0, 0, 0
}
