package ycmd

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The synthetic model mirrors the real graph in miniature: 40 log-mel
// inputs -> cmvn add -> cmvn mul -> affine (2x40, no bias) -> conv (2
// channels x 3 taps, bias, context 1) -> hardswish -> affine (4x2 + bias)
// -> softmax. Weights are made up: a few low mel bins push towards
// posterior 1, a few high ones towards posterior 2, quiet gives SIL.
var (
	synthAdd  = fill(40, func(i int) float32 { return -8 - 5.5*float32(i/20) })
	synthMul  = fill(40, func(int) float32 { return 0.25 })
	synthFeat = fillQ(2*40, func(i int) int16 { // row 0 hears bins 2..7, row 1 bins 23..28
		if b := i % 40; b >= 2+21*(i/40) && b < 8+21*(i/40) {
			return 800
		}
		return 0
	})
	synthFeatS = float32(1.0 / 4096)
	synthConvQ = []int16{1000, 2000, 3000, -1000, 4000, 2000} // channel-major, tap 0 = oldest
	synthConvS = float32(1.0 / 8192)
	synthConvB = []float32{0.2, 0.25}
	synthHeadQ = []int16{0, 0, 6000, -6000, -6000, 6000, 0, 0}
	synthHeadS = float32(1.0 / 1024)
	synthHeadB = []float32{1, 0, 0, -1}
)

func fill(n int, f func(int) float32) []float32 {
	v := make([]float32, n)
	for i := range v {
		v[i] = f(i)
	}
	return v
}

func fillQ(n int, f func(int) int16) []int16 {
	v := make([]int16, n)
	for i := range v {
		v[i] = f(i)
	}
	return v
}

func le32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }

func i32(v int) []byte { return append([]byte{4}, le32(uint32(v))...) }

func fv(v []float32) []byte {
	b := append([]byte("FV "), i32(len(v))...)
	for _, f := range v {
		b = append(b, le32(math.Float32bits(f))...)
	}
	return b
}

// lin is "<S> s <LinearParams> SIM rows cols q...".
func lin(s float32, rows, cols int, q []int16) []byte {
	b := append([]byte("<S> \x04"), le32(math.Float32bits(s))...)
	b = append(b, "<LinearParams> SIM "...)
	b = append(append(b, i32(rows)...), i32(cols)...)
	for _, v := range q {
		b = binary.LittleEndian.AppendUint16(b, uint16(v))
	}
	return b
}

type synthComp struct {
	name, typ string
	body      []byte
}

func synthComps() []synthComp {
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	return []synthComp{
		{"add", "FixedBiasComponent", cat([]byte("<Bias> "), fv(synthAdd))},
		{"mul", "FixedScaleComponent", cat([]byte("<Scales> "), fv(synthMul))},
		{"feat", "Q16FixedAffineComponent", lin(synthFeatS, 2, 40, synthFeat)},
		{"times", "Q16GroupConvCorouComponent", cat(lin(synthConvS, 2, 3, synthConvQ),
			[]byte("<BiasParams> "), fv(synthConvB), []byte("<ContextParam> "), i32(1))},
		{"hs", "hardswish", nil},
		{"head", "Q16FixedAffineComponent", cat(lin(synthHeadS, 4, 2, synthHeadQ), []byte("<BiasParams> "), fv(synthHeadB))},
		{"sm", "SoftmaxComponent", cat([]byte("<Dim> "), i32(4),
			[]byte("<ValueAvg> FV "), i32(0), []byte("<Count> \x08\x00\x00\x00\x00\x00\x00\x00\x00"))},
	}
}

// synthNet builds a tiny nnet3 file in the layout of the real model:
// text config, then binary components.
func synthNet(config string, comps []synthComp) []byte {
	var b bytes.Buffer
	b.WriteString(config)
	b.WriteString("<NumComponents> ")
	b.Write(i32(len(comps)))
	for _, c := range comps {
		fmt.Fprintf(&b, "<ComponentName> %s <%s> ", c.name, c.typ)
		b.Write(c.body)
		fmt.Fprintf(&b, "</%s> ", c.typ)
	}
	b.WriteString("</Nnet3> \n")
	return b.Bytes()
}

const synthConfig = " B<Nnet3> \n" +
	"input-node name=input dim=40\n" +
	"component-node name=add component=add input=input\n" +
	"component-node name=mul component=mul input=add\n" +
	"component-node name=feat component=feat input=mul\n" +
	"component-node name=times component=times input=feat\n" +
	"component-node name=hs component=hs input=times\n" +
	"component-node name=head component=head input=hs\n" +
	"component-node name=sm component=sm input=head\n" +
	"output-node name=output input=sm objective=linear\n\n"

const synthFlags = "commands 1:2\nthresholds 0.5:0.6\nw-smooth 8\nw-max 60\n" +
	"interval-after-last-activation 25\nversion synth-v1\nsample-rate 16000\n"

const synthWords = "SIL 0\nвключи 1\nсвет 2\nтихо 3\n"

func writeModel(t *testing.T, net []byte, flags string) string {
	t.Helper()
	return writeModelWords(t, net, flags, synthWords)
}

func writeModelWords(t *testing.T, net []byte, flags, words string) string {
	t.Helper()
	dir := t.TempDir()
	for name, data := range map[string]string{
		"acoustic_model.nnet": string(net),
		"flags.txt":           flags,
		"words.txt":           words,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoadMissing(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "acoustic_model.nnet") {
		t.Fatalf("empty dir: err = %v, want one naming acoustic_model.nnet", err)
	}
	for _, name := range []string{"flags.txt", "words.txt"} {
		d := writeModel(t, synthNet(synthConfig, synthComps()), synthFlags)
		os.Remove(filepath.Join(d, name))
		if _, err := Load(d); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("without %s: err = %v", name, err)
		}
	}
}

func TestLoadSynthetic(t *testing.T) {
	m, err := Load(writeModel(t, synthNet(synthConfig, synthComps()), synthFlags))
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != "synth-v1" || m.WSmooth != 8 || m.WMax != 60 || m.Rearm != 25 {
		t.Errorf("flags: %q %d %d %d", m.Version, m.WSmooth, m.WMax, m.Rearm)
	}
	if len(m.Commands) != 2 {
		t.Fatalf("commands = %+v", m.Commands)
	}
	if c := m.Commands[0]; c.Name != "включи" || len(c.Labels) != 1 || c.Labels[0] != 1 || c.Threshold != 0.5 {
		t.Errorf("command 0 = %+v", c)
	}
	if c := m.Commands[1]; c.Name != "свет" || c.Labels[0] != 2 || c.Threshold != 0.6 {
		t.Errorf("command 1 = %+v", c)
	}
	if len(m.net) != 7 {
		t.Fatalf("net has %d nodes, want 7", len(m.net))
	}
	kinds := []compKind{kindBias, kindScale, kindAffine, kindConv, kindHardswish, kindAffine, kindSoftmax}
	for i, n := range m.net {
		if n.kind != kinds[i] || n.input != i-1 {
			t.Errorf("node %d (%s): kind %d input %d, want %d %d", i, n.name, n.kind, n.input, kinds[i], i-1)
		}
	}
	eq := func(what string, got, want []float32) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: %d values, want %d", what, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s[%d] = %v, want %v", what, i, got[i], want[i])
			}
		}
	}
	deq := func(q []int16, s float32) []float32 {
		return fill(len(q), func(i int) float32 { return float32(q[i]) * s })
	}
	eq("cmvn bias", m.net[0].bias, synthAdd)
	eq("cmvn scales", m.net[1].scales, synthMul)
	if f := m.net[2]; f.rows != 2 || f.cols != 40 || f.bias != nil || f.dim != 2 {
		t.Errorf("feat: %dx%d bias %v dim %d", f.rows, f.cols, f.bias, f.dim)
	}
	eq("feat w", m.net[2].w, deq(synthFeat, synthFeatS))
	// Conv: channels x taps, row-major, tap 0 first.
	c := m.net[3]
	if c.rows != 2 || c.cols != 3 || c.context != 1 || c.dim != 2 {
		t.Errorf("conv: %dx%d context %d dim %d", c.rows, c.cols, c.context, c.dim)
	}
	eq("conv w", c.w, deq(synthConvQ, synthConvS))
	eq("conv bias", c.bias, synthConvB)
	if h := m.net[4]; h.dim != 2 {
		t.Errorf("hardswish dim %d, want 2", h.dim)
	}
	eq("head w", m.net[5].w, deq(synthHeadQ, synthHeadS))
	eq("head bias", m.net[5].bias, synthHeadB)
	if sm := m.net[6]; sm.dim != 4 {
		t.Errorf("softmax dim %d", sm.dim)
	}
}

func TestLoadTruncated(t *testing.T) {
	net := synthNet(synthConfig, synthComps())
	// Half the file, then every other cut point: always an error, never a panic.
	for _, n := range append([]int{len(net) / 2}, cutPoints(len(net))...) {
		if _, err := Load(writeModel(t, net[:n], synthFlags)); err == nil {
			t.Fatalf("cut at %d of %d: no error", n, len(net))
		}
	}
}

func cutPoints(n int) []int {
	var c []int
	for i := 0; i < n-1; i++ {
		c = append(c, i)
	}
	return c
}

func TestLoadBadModels(t *testing.T) {
	narrow := synthComps()
	narrow[2].body = lin(synthFeatS, 2, 39, synthFeat[:78])
	unknown := bytes.Replace(synthNet(synthConfig, synthComps()), []byte("<SoftmaxComponent>"), []byte("<WeirdComponent>"), 1)
	noS := bytes.Replace(synthNet(synthConfig, synthComps()), lin(synthConvS, 2, 3, synthConvQ)[:9], nil, 1)
	cases := []struct {
		name, want string
		net        []byte
		flags      string
	}{
		{"layer width mismatch", "layer feat", synthNet(synthConfig, narrow), synthFlags},
		{"weights without scale", "<LinearParams> without <S>", noS, synthFlags},
		{"unknown component", "WeirdComponent", unknown, synthFlags},
		{"label out of range", "label", synthNet(synthConfig, synthComps()), strings.Replace(synthFlags, "1:2", "1:7", 1)},
		{"threshold count", "thresholds", synthNet(synthConfig, synthComps()), strings.Replace(synthFlags, "0.5:0.6", "0.5", 1)},
		{"no w-max", "w-max", synthNet(synthConfig, synthComps()), strings.Replace(synthFlags, "w-max 60\n", "", 1)},
	}
	for _, c := range cases {
		_, err := Load(writeModel(t, c.net, c.flags))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one containing %q", c.name, err, c.want)
		}
	}
	// The network must emit one posterior per word.
	_, err := Load(writeModelWords(t, synthNet(synthConfig, synthComps()), synthFlags, "SIL 0\nвключи 1\nсвет 2\n"))
	if err == nil || !strings.Contains(err.Error(), "4 posteriors") {
		t.Errorf("word count: err = %v", err)
	}
	// The input must be the 40 log-mel bins the frontend makes.
	in20 := strings.Replace(synthConfig, "dim=40", "dim=20", 1)
	_, err = Load(writeModel(t, synthNet(in20, synthComps()), synthFlags))
	if err == nil || !strings.Contains(err.Error(), "input dim 20") {
		t.Errorf("input width 20: err = %v", err)
	}
}

func TestLoadReal(t *testing.T) {
	dir := os.Getenv("YCMD_DIR")
	if dir == "" {
		t.Skip("YCMD_DIR not set")
	}
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Version != TestedVersion {
		t.Errorf("version = %q", m.Version)
	}
	if len(m.Commands) != 23 {
		t.Errorf("%d commands, want 23", len(m.Commands))
	}
	have := map[string]bool{}
	for _, c := range m.Commands {
		have[c.Name] = true
	}
	for _, want := range []string{"стоп", "продолжи", "включи блютуз"} {
		if !have[want] {
			t.Errorf("no command %q", want)
		}
	}
	if m.WSmooth != 8 || m.WMax != 60 || m.Rearm != 25 {
		t.Errorf("decoder flags: %d %d %d", m.WSmooth, m.WMax, m.Rearm)
	}
	// Two softmax heads (22 + 4 posteriors) sit at the end of the graph.
	n := len(m.net)
	if n < 2 || m.net[n-2].kind != kindSoftmax || m.net[n-2].dim != 22 || m.net[n-1].dim != 4 {
		t.Errorf("heads: %d nodes, tail %+v", n, m.net[max(n-2, 0):])
	}
	convs := 0
	for _, nd := range m.net {
		if nd.kind == kindConv {
			convs++
			if prev := m.net[nd.input]; nd.rows != prev.dim || len(nd.bias) != nd.rows || nd.cols < 2 {
				t.Errorf("conv %s: %dx%d after width %d, %d biases", nd.name, nd.rows, nd.cols, prev.dim, len(nd.bias))
			}
		}
	}
	if convs != 5 {
		t.Errorf("%d conv layers, want 5", convs)
	}
}
