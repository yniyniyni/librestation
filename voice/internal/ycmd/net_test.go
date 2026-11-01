package ycmd

import (
	"math"
	"os"
	"path/filepath"
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

// runBatches feeds feats to n in batches of b frames and returns the
// posteriors, one slice per frame that produced output.
func runBatches(n *nnet, feats [][numMel]float32, b int) [][]float32 {
	var got [][]float32
	flush := func() {
		p := n.run()
		for len(p) > 0 {
			got = append(got, append([]float32(nil), p[:n.nPost]...))
			p = p[n.nPost:]
		}
	}
	for _, f := range feats {
		if n.add(f[:]) || n.nin == b {
			flush()
		}
	}
	if n.nin > 0 {
		flush()
	}
	return got
}

func TestNetSynthetic(t *testing.T) {
	m := synthModel(t)
	// Long enough for every conv history to wrap around several times.
	var feats [][numMel]float32
	for k := range 100 {
		var f [numMel]float32
		for i := range f {
			f[i] = 12 + float32(k%9*k%9)*0.7
			if i >= 20 {
				f[i] = 12 - float32(k%7)*1.3
			}
		}
		feats = append(feats, f)
	}
	want := refPosteriors(feats)
	n := newNnet(m.net)
	for b := 1; b <= maxBatch; b++ {
		got := runBatches(n, feats, b)
		if len(got) != len(want) {
			t.Fatalf("batches of %d: %d outputs, want %d (first dropped)", b, len(got), len(want))
		}
		for i := range want {
			for j := range want[i] {
				if math.Abs(float64(got[i][j])-want[i][j]) > 1e-5 {
					t.Errorf("batches of %d: frame %d posterior %d = %v, want %v", b, i, j, got[i][j], want[i][j])
				}
			}
		}
		n.reset() // every pass starts from silence again
	}
}

// naivePosteriors evaluates m's graph frame by frame in the plainest way,
// straight from the parsed weights: the reference for the blocked loops.
func naivePosteriors(m *Model, feats [][numMel]float32) [][]float32 {
	net := m.net
	hist := make([][][]float32, len(net)) // every input a conv has seen
	var out [][]float32
	for _, feat := range feats {
		y := make([][]float32, len(net))
		dropped := false
		for i, nd := range net {
			x := feat[:]
			if nd.input >= 0 {
				x = y[nd.input]
			}
			o := make([]float32, nd.dim)
			switch nd.kind {
			case kindBias:
				for j := range o {
					o[j] = x[j] + nd.bias[j]
				}
			case kindScale:
				for j := range o {
					o[j] = x[j] * nd.scales[j]
				}
			case kindAffine:
				for r := range nd.rows {
					var acc float32
					for k := range nd.cols {
						acc += nd.w[r*nd.cols+k] * x[k]
					}
					if nd.bias != nil {
						acc += nd.bias[r]
					}
					o[r] = acc
				}
			case kindConv:
				hist[i] = append(hist[i], x)
				h := hist[i]
				for c := range nd.rows {
					var acc float32
					for k := range nd.cols {
						if s := len(h) - nd.cols + k; s >= 0 {
							acc += nd.w[c*nd.cols+k] * h[s][c]
						}
					}
					o[c] = acc + nd.bias[c]
				}
				if len(h) <= nd.context {
					dropped = true // later layers never see this frame
				}
			case kindHardswish:
				for j, v := range x {
					o[j] = v * min(max(v+3, 0), 6) / 6
				}
			case kindSoftmax:
				softmax(x, o)
			}
			y[i] = o
			if dropped {
				break
			}
		}
		if dropped {
			continue
		}
		var p []float32
		for _, h := range heads(net) {
			p = append(p, y[h]...)
		}
		out = append(out, p)
	}
	return out
}

// TestNetRealClip checks the network against naivePosteriors on a real
// recording, frame by frame, for every batch size.
func TestNetRealClip(t *testing.T) {
	dir, clips := os.Getenv("YCMD_DIR"), os.Getenv("YCMD_CLIPS")
	if dir == "" || clips == "" {
		t.Skip("YCMD_DIR or YCMD_CLIPS not set")
	}
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	pcm := rawCh0(t, filepath.Join(clips, "near.wav"))
	var fb fbank
	fb.init()
	var feats [][numMel]float32
	for len(pcm) > 0 {
		k, ok := fb.fill(pcm)
		pcm = pcm[k:]
		if ok {
			var f [numMel]float32
			fb.frame(f[:])
			feats = append(feats, f)
		}
	}
	want := naivePosteriors(m, feats)
	n := newNnet(m.net)
	for b := 1; b <= maxBatch; b++ {
		got := runBatches(n, feats, b)
		if len(got) != len(want) {
			t.Fatalf("batches of %d: %d outputs, want %d", b, len(got), len(want))
		}
		worst := 0.0
		for i := range want {
			for j := range want[i] {
				worst = max(worst, math.Abs(float64(got[i][j]-want[i][j])))
			}
		}
		t.Logf("batches of %d: %d frames, max |posterior diff| %.3g", b, len(got), worst)
		if worst > 1e-4 {
			t.Errorf("batches of %d: posteriors differ by %g, want <= 1e-4", b, worst)
		}
		n.reset()
	}
}
