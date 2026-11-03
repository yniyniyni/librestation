package ycmd

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func writeBFM(t *testing.T, rows, cols int, v func(r, c int) float32) string {
	t.Helper()
	b := []byte("\x00BFM \x04")
	b = binary.LittleEndian.AppendUint32(b, uint32(rows))
	b = append(b, 4)
	b = binary.LittleEndian.AppendUint32(b, uint32(cols))
	for r := range rows {
		for c := range cols {
			b = binary.LittleEndian.AppendUint32(b, math.Float32bits(v(r, c)))
		}
	}
	p := filepath.Join(t.TempDir(), "lda.mat")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadLDA(t *testing.T) {
	cols := mfccCeps * (2*ldaContext + 1)
	got, m, err := loadLDA(writeBFM(t, numMel, cols, func(r, c int) float32 { return float32(r*1000 + c) }))
	if err != nil || got != cols {
		t.Fatalf("cols %d, err %v", got, err)
	}
	if m[1*cols+2] != 1002 {
		t.Errorf("row 1, column 2 = %v", m[1*cols+2])
	}
	if _, _, err := loadLDA(writeBFM(t, 3, 3, func(r, c int) float32 { return 0 })); err == nil {
		t.Error("a matrix of another shape must be refused")
	}
}

// The first output comes with the right context in; the mean is taken off, so
// a steady input gives zeros whatever its level.
func TestLDAFrontSpliceAndMean(t *testing.T) {
	cols := mfccCeps * (2*ldaContext + 1)
	lda := make([]float32, numMel*cols)
	for r := range numMel {
		lda[r*cols] = 1
	}
	f := newLDAFront(lda, cols, 100)
	mel := make([]float32, mfccBins)
	for i := range mel {
		mel[i] = 5
	}
	out := make([]float32, numMel)
	for i := range ldaContext {
		if f.push(mel, 7, out) {
			t.Fatalf("output at frame %d, before the right context", i)
		}
	}
	if !f.push(mel, 7, out) {
		t.Fatal("no output with the context in")
	}
	if math.Abs(float64(out[0])) > 1e-4 {
		t.Errorf("steady input gives %v", out[0])
	}
}

func TestRelu(t *testing.T) {
	nd := []*node{{name: "r", kind: kindRelu, input: -1, dim: numMel}}
	n := newNnet(nd)
	in := make([]float32, numMel)
	in[0], in[1] = -2, 3
	n.add(in)
	p := n.run()
	if p[0] != 0 || p[1] != 3 {
		t.Errorf("got %v", p[:2])
	}
}

func TestDitherChangesSilenceOnly(t *testing.T) {
	var f fbank
	f.init()
	f.dither, f.rng = 0.1, 2463534242
	zero := make([]float32, frameLen)
	f.fill(zero)
	var e float64
	for _, v := range f.x {
		e = math.Max(e, math.Abs(v))
	}
	if e == 0 || e > 0.5 {
		t.Errorf("largest noise %v, want small and not zero", e)
	}
}
