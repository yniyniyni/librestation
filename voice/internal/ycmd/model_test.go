package ycmd

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Made-up affine layer: 4 outputs, 3 inputs, one scale for the matrix.
var (
	synthQ    = []int16{100, -200, 300, 400, 500, -600, -700, 800, 900, 1000, -1100, 1200}
	synthS    = float32(0.25)
	synthBias = []float32{0.5, -0.5, 1, -1}
)

func le32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }

func fv(v []float32) []byte {
	b := append([]byte("FV \x04"), le32(uint32(len(v)))...)
	for _, f := range v {
		b = append(b, le32(math.Float32bits(f))...)
	}
	return b
}

// synthNet builds a tiny nnet3 file in the layout of the real model:
// text config, then binary components.
func synthNet(config string) []byte {
	var b bytes.Buffer
	b.WriteString(config)
	b.WriteString("<NumComponents> \x04" + string(le32(2)))

	b.WriteString("<ComponentName> aff <Q16FixedAffineComponent> <S> \x04")
	b.Write(le32(math.Float32bits(synthS)))
	b.WriteString("<LinearParams> SIM \x04")
	b.Write(le32(4))
	b.WriteString("\x04")
	b.Write(le32(3))
	for _, q := range synthQ {
		b.Write(binary.LittleEndian.AppendUint16(nil, uint16(q)))
	}
	b.WriteString("<BiasParams> ")
	b.Write(fv(synthBias))
	b.WriteString("</Q16FixedAffineComponent> ")

	b.WriteString("<ComponentName> sm <SoftmaxComponent> <Dim> \x04")
	b.Write(le32(4))
	b.WriteString("<ValueAvg> FV \x04\x00\x00\x00\x00<Count> \x08\x00\x00\x00\x00\x00\x00\x00\x00</SoftmaxComponent> </Nnet3> \n")
	return b.Bytes()
}

const synthConfig = " B<Nnet3> \n" +
	"input-node name=input dim=3\n" +
	"component-node name=aff component=aff input=input\n" +
	"component-node name=sm component=sm input=aff\n" +
	"output-node name=output input=sm objective=linear\n\n"

const synthFlags = "commands 1:2\nthresholds 0.5:0.6\nw-smooth 8\nw-max 60\n" +
	"interval-after-last-activation 25\nversion synth-v1\nsample-rate 16000\n"

func writeModel(t *testing.T, net []byte, flags string) string {
	t.Helper()
	dir := t.TempDir()
	for name, data := range map[string]string{
		"acoustic_model.nnet": string(net),
		"flags.txt":           flags,
		"words.txt":           "SIL 0\nвключи 1\nсвет 2\n",
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
		d := writeModel(t, synthNet(synthConfig), synthFlags)
		os.Remove(filepath.Join(d, name))
		if _, err := Load(d); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("without %s: err = %v", name, err)
		}
	}
}

func TestLoadSynthetic(t *testing.T) {
	m, err := Load(writeModel(t, synthNet(synthConfig), synthFlags))
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
	if len(m.net) != 2 {
		t.Fatalf("net has %d nodes, want 2", len(m.net))
	}
	aff := m.net[0]
	if aff.kind != kindAffine || aff.rows != 4 || aff.cols != 3 || aff.input != -1 {
		t.Errorf("affine node = %+v", aff)
	}
	for i, q := range synthQ {
		if want := float32(q) * synthS; aff.w[i] != want {
			t.Errorf("w[%d] = %v, want %v", i, aff.w[i], want)
		}
	}
	for i, b := range synthBias {
		if aff.bias[i] != b {
			t.Errorf("bias[%d] = %v, want %v", i, aff.bias[i], b)
		}
	}
	if sm := m.net[1]; sm.kind != kindSoftmax || sm.input != 0 {
		t.Errorf("softmax node = %+v", sm)
	}
}

func TestLoadTruncated(t *testing.T) {
	net := synthNet(synthConfig)
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
	dim := strings.Replace(synthConfig, "dim=3", "dim=5", 1)
	unknown := bytes.Replace(synthNet(synthConfig), []byte("<SoftmaxComponent>"), []byte("<WeirdComponent>"), 1)
	cases := []struct {
		name, want string
		net        []byte
		flags      string
	}{
		{"layer width mismatch", "aff", synthNet(dim), synthFlags},
		{"unknown component", "WeirdComponent", unknown, synthFlags},
		{"label out of range", "label", synthNet(synthConfig), strings.Replace(synthFlags, "1:2", "1:7", 1)},
		{"threshold count", "thresholds", synthNet(synthConfig), strings.Replace(synthFlags, "0.5:0.6", "0.5", 1)},
		{"no w-max", "w-max", synthNet(synthConfig), strings.Replace(synthFlags, "w-max 60\n", "", 1)},
	}
	for _, c := range cases {
		_, err := Load(writeModel(t, c.net, c.flags))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one containing %q", c.name, err, c.want)
		}
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
}
