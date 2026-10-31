package ycmd

import (
	"math"
	"testing"
)

func synthModel(t *testing.T) *Model {
	t.Helper()
	m, err := Load(writeModel(t, synthNet(synthConfig, synthComps()), synthFlags))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// refPosteriors is the synthetic network written out by hand for a whole
// feature sequence: the conv's tap k multiplies frame t-2+k (tap 0 is the
// oldest), frames before the start are zero, and ContextParam 1 drops the
// first output.
func refPosteriors(feats [][numMel]float32) [][]float64 {
	var feat [][2]float64
	for _, f := range feats {
		var x [2]float64
		for r := range 2 {
			for i := range numMel {
				v := (float64(f[i]) + float64(synthAdd[i])) * float64(synthMul[i])
				x[r] += float64(synthFeat[r*40+i]) * float64(synthFeatS) * v
			}
		}
		feat = append(feat, x)
	}
	var out [][]float64
	for t := 1; t < len(feat); t++ {
		var h [2]float64
		for ch := range 2 {
			y := float64(synthConvB[ch])
			for k := range 3 {
				if s := t - 2 + k; s >= 0 {
					y += float64(synthConvQ[ch*3+k]) * float64(synthConvS) * feat[s][ch]
				}
			}
			h[ch] = y * min(max(y+3, 0), 6) / 6
		}
		logits := make([]float64, 4)
		sum := 0.0
		for r := range 4 {
			logits[r] = float64(synthHeadB[r])
			for c := range 2 {
				logits[r] += float64(synthHeadQ[r*2+c]) * float64(synthHeadS) * h[c]
			}
			logits[r] = math.Exp(logits[r])
			sum += logits[r]
		}
		for r := range logits {
			logits[r] /= sum
		}
		out = append(out, logits)
	}
	return out
}

func TestNetSynthetic(t *testing.T) {
	m := synthModel(t)
	var feats [][numMel]float32
	for k := range 6 {
		var f [numMel]float32
		for i := range f {
			f[i] = 12 + float32(k*k)*0.7
			if i >= 20 {
				f[i] = 12 - float32(k)*1.3
			}
		}
		feats = append(feats, f)
	}
	want := refPosteriors(feats)
	n := newNnet(m.net)
	for pass := range 2 {
		var got [][]float32
		for _, f := range feats {
			if p, ok := n.step(f[:]); ok {
				got = append(got, append([]float32(nil), p...))
			}
		}
		if len(got) != len(want) {
			t.Fatalf("pass %d: %d outputs, want %d (first dropped)", pass, len(got), len(want))
		}
		for i := range want {
			for j := range want[i] {
				if math.Abs(float64(got[i][j])-want[i][j]) > 1e-5 {
					t.Errorf("pass %d frame %d posterior %d = %v, want %v", pass, i, j, got[i][j], want[i][j])
				}
			}
		}
		n.reset() // the second pass must match the first
	}
}
