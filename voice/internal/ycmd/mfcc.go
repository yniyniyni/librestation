package ycmd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
)

// The Alisa model's front end (feat-pipeline cmn:lda): Kaldi's MFCC, mean
// normalised over a sliding window, seven frames spliced and turned into the
// network's 40 inputs by the matrix in lda.mat. The other models read log-mel
// directly.
const (
	mfccBins   = 23
	mfccCeps   = 13
	mfccLifter = 22.0
	ldaContext = 3 // frames spliced on each side
)

// loadLDA reads a Kaldi binary float matrix ("\0BFM ").
func loadLDA(path string) (cols int, m []float32, err error) {
	d, err := os.ReadFile(path)
	if err != nil {
		return 0, nil, err
	}
	if len(d) < 15 || string(d[:5]) != "\x00BFM " {
		return 0, nil, errors.New("not a Kaldi binary float matrix")
	}
	rows, cols := int(int32(binary.LittleEndian.Uint32(d[6:]))), int(int32(binary.LittleEndian.Uint32(d[11:])))
	if rows != numMel || cols != mfccCeps*(2*ldaContext+1) || len(d) != 15+4*rows*cols {
		return 0, nil, fmt.Errorf("matrix %dx%d, want %dx%d", rows, cols, numMel, mfccCeps*(2*ldaContext+1))
	}
	m = make([]float32, rows*cols)
	for i := range m {
		m[i] = math.Float32frombits(binary.LittleEndian.Uint32(d[15+4*i:]))
	}
	return cols, m, nil
}

// ldaFront turns log-mel frames plus the frame's log energy into the network's
// input. It holds cmnWindow frames for the mean and the splice's history.
type ldaFront struct {
	dct    [mfccCeps][mfccBins]float64
	lifter [mfccCeps]float64
	lda    []float32 // numMel x cols
	cols   int

	cmn     [][mfccCeps]float32 // ring of the last frames, before normalisation
	cmnSum  [mfccCeps]float64
	cmnN    int // frames in the ring
	cmnAt   int
	spliced [2*ldaContext + 1][mfccCeps]float32
	seen    int
}

func newLDAFront(lda []float32, cols, cmnWindow int) *ldaFront {
	f := &ldaFront{lda: lda, cols: cols, cmn: make([][mfccCeps]float32, cmnWindow)}
	for k := range mfccCeps {
		norm := math.Sqrt(2.0 / mfccBins)
		if k == 0 {
			norm = math.Sqrt(1.0 / mfccBins)
		}
		for n := range mfccBins {
			f.dct[k][n] = norm * math.Cos(math.Pi/mfccBins*(float64(n)+0.5)*float64(k))
		}
		f.lifter[k] = 1 + 0.5*mfccLifter*math.Sin(math.Pi*float64(k)/mfccLifter)
	}
	return f
}

func (f *ldaFront) reset() {
	clear(f.cmnSum[:])
	f.cmnN, f.cmnAt, f.seen = 0, 0, 0
}

// push takes one frame's log-mel and log energy; out gets the network's input
// once the splice has its right context, and push reports whether it has.
func (f *ldaFront) push(mel []float32, energy float64, out []float32) bool {
	var c [mfccCeps]float32
	for k := range mfccCeps {
		var s float64
		for n, m := range mel[:mfccBins] {
			s += f.dct[k][n] * float64(m)
		}
		c[k] = float32(s * f.lifter[k])
	}
	c[0] = float32(energy) // use-energy: Kaldi's default
	if f.cmnN == len(f.cmn) {
		for k, v := range f.cmn[f.cmnAt] {
			f.cmnSum[k] -= float64(v)
		}
	} else {
		f.cmnN++
	}
	f.cmn[f.cmnAt] = c
	f.cmnAt = (f.cmnAt + 1) % len(f.cmn)
	for k, v := range c {
		f.cmnSum[k] += float64(v)
	}
	for k := range c {
		c[k] -= float32(f.cmnSum[k] / float64(f.cmnN))
	}
	if f.seen == 0 {
		for i := range f.spliced {
			f.spliced[i] = c // the first frame stands in for what came before
		}
	}
	copy(f.spliced[:], f.spliced[1:])
	f.spliced[len(f.spliced)-1] = c
	f.seen++
	if f.seen <= ldaContext {
		return false
	}
	for r := range numMel {
		row := f.lda[r*f.cols : (r+1)*f.cols]
		var s float32
		for i, fr := range f.spliced {
			for k, v := range fr {
				s += row[i*mfccCeps+k] * v
			}
		}
		out[r] = s
	}
	return true
}
