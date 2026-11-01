// Package ycmd runs Yandex's stock quick-command spotter (the "command"
// model shipped in the speaker's own firmware) inside voiced.
package ycmd

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// TestedVersion is the model build this package was developed and verified
// against; Load accepts others (the format is the same) but callers may warn.
const TestedVersion = "ru-RU-quasar-commands-29Nov24-08-35-retune-v2-shrd-bt-q16-rt"

// Command is one phrase the model can report: a single word or an ordered
// pair such as "включи свет".
type Command struct {
	Name      string  // words joined by a space
	Labels    []int   // posterior ids, in spoken order
	Threshold float64 // confidence needed to fire
}

// Model is a parsed command model: the network and the decoder settings.
type Model struct {
	Version  string
	Commands []Command
	WSmooth  int // posterior smoothing window, frames
	WMax     int // window for the max of the smoothed posterior, frames
	Rearm    int // frames to stay quiet after a hit

	net []*node // in graph order; every input refers to an earlier index
}

// compKind is the component types the reader understands.
type compKind int

const (
	kindBias      compKind = iota // FixedBiasComponent: x + bias
	kindScale                     // FixedScaleComponent: x * scales
	kindAffine                    // Q16FixedAffineComponent: w*x (+ bias)
	kindConv                      // Q16GroupConvCorouComponent: streaming depthwise FIR
	kindHardswish                 // hardswish
	kindSoftmax                   // SoftmaxComponent
)

var kindByType = map[string]compKind{
	"FixedBiasComponent":         kindBias,
	"FixedScaleComponent":        kindScale,
	"Q16FixedAffineComponent":    kindAffine,
	"Q16GroupConvCorouComponent": kindConv,
	"hardswish":                  kindHardswish,
	"SoftmaxComponent":           kindSoftmax,
}

// node is one component-node of the graph with its parameters.
//
// Affine: w is rows x cols row-major (out x in). Conv: w is rows (channels)
// x cols (taps, tap 0 = oldest of the last cols frames); context is the
// number of initial output frames the model drops.
type node struct {
	name       string
	kind       compKind
	input      int       // index in Model.net of the producing node, -1 for the network input
	rows, cols int       // matrix shape (affine, conv)
	w          []float32 // dequantised weights, q * scale
	bias       []float32
	scales     []float32
	context    int
	dim        int // output width
}

// Load reads acoustic_model.nnet, flags.txt and words.txt from dir.
func Load(dir string) (*Model, error) {
	data, err := os.ReadFile(filepath.Join(dir, "acoustic_model.nnet"))
	if err != nil {
		return nil, fmt.Errorf("ycmd: %w", err)
	}
	flags, err := loadFlags(filepath.Join(dir, "flags.txt"))
	if err != nil {
		return nil, err
	}
	words, err := loadWords(filepath.Join(dir, "words.txt"))
	if err != nil {
		return nil, err
	}
	m := &Model{Version: flags["version"]}
	if m.Version == "" {
		return nil, errors.New("ycmd: flags.txt: no version")
	}
	if m.Commands, err = parseCommands(flags, words); err != nil {
		return nil, err
	}
	for _, f := range []struct {
		key string
		dst *int
	}{{"w-smooth", &m.WSmooth}, {"w-max", &m.WMax}, {"interval-after-last-activation", &m.Rearm}} {
		v, err := strconv.Atoi(flags[f.key])
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("ycmd: flags.txt: bad %s %q", f.key, flags[f.key])
		}
		*f.dst = v
	}
	if m.net, err = parseNnet(data); err != nil {
		return nil, fmt.Errorf("ycmd: acoustic_model.nnet: %w", err)
	}
	if n := posteriors(m.net); n != len(words) {
		return nil, fmt.Errorf("ycmd: acoustic_model.nnet gives %d posteriors, words.txt has %d words", n, len(words))
	}
	return m, nil
}

func loadFlags(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("ycmd: %w", err)
	}
	defer f.Close()
	flags := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, _ := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if k != "" {
			flags[k] = strings.TrimSpace(v)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("ycmd: %s: %w", path, err)
	}
	return flags, nil
}

// loadWords returns the word of every posterior id; a line is "word id".
func loadWords(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ycmd: %w", err)
	}
	var words []string
	for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		w, id, _ := strings.Cut(strings.TrimSpace(line), " ")
		if n, err := strconv.Atoi(id); w == "" || err != nil || n != i {
			return nil, fmt.Errorf("ycmd: words.txt line %d: want %q, got %q", i+1, "word "+strconv.Itoa(i), line)
		}
		words = append(words, w)
	}
	return words, nil
}

// parseCommands reads "commands 1:2:3,4" (a comma joins the labels of one
// command) and the matching thresholds.
func parseCommands(flags map[string]string, words []string) ([]Command, error) {
	specs := strings.Split(flags["commands"], ":")
	thr := strings.Split(flags["thresholds"], ":")
	if flags["commands"] == "" {
		return nil, errors.New("ycmd: flags.txt: no commands")
	}
	if len(thr) != len(specs) {
		return nil, fmt.Errorf("ycmd: flags.txt: %d commands but %d thresholds", len(specs), len(thr))
	}
	cmds := make([]Command, len(specs))
	for i, spec := range specs {
		c := &cmds[i]
		var names []string
		for _, p := range strings.Split(spec, ",") {
			id, err := strconv.Atoi(p)
			if err != nil || id < 0 || id >= len(words) {
				return nil, fmt.Errorf("ycmd: flags.txt: command %q: label %q out of range (%d words)", spec, p, len(words))
			}
			c.Labels = append(c.Labels, id)
			names = append(names, words[id])
		}
		c.Name = strings.Join(names, " ")
		var err error
		if c.Threshold, err = strconv.ParseFloat(thr[i], 64); err != nil {
			return nil, fmt.Errorf("ycmd: flags.txt: threshold %q: %w", thr[i], err)
		}
	}
	return cmds, nil
}

// nnetReader walks the Kaldi nnet3 binary. The first problem sticks in err
// and every later read returns zero values, so callers check once.
type nnetReader struct {
	d   []byte
	i   int
	err error
}

func (r *nnetReader) fail(format string, a ...any) {
	if r.err == nil {
		r.err = fmt.Errorf("at byte %d: "+format, append([]any{r.i}, a...)...)
	}
}

// tok returns the next space-terminated token.
func (r *nnetReader) tok() string {
	for r.i < len(r.d) && r.d[r.i] == ' ' {
		r.i++
	}
	j := bytes.IndexByte(r.d[min(r.i, len(r.d)):], ' ')
	if r.err != nil || j < 0 {
		r.fail("unexpected end of file")
		return ""
	}
	t := string(r.d[r.i : r.i+j])
	r.i += j + 1
	return t
}

// raw takes the next n bytes.
func (r *nnetReader) raw(n int) []byte {
	if r.err != nil || n < 0 || n > len(r.d)-r.i {
		r.fail("unexpected end of file")
		return nil
	}
	b := r.d[r.i : r.i+n]
	r.i += n
	return b
}

// sized takes a 4-byte value that Kaldi prefixes with its size byte.
func (r *nnetReader) sized() []byte {
	if b := r.raw(1); b != nil && b[0] != 4 {
		r.i--
		r.fail("expected size byte 4, got %d", b[0])
		return nil
	}
	return r.raw(4)
}

func (r *nnetReader) int32() int {
	if b := r.sized(); b != nil {
		return int(int32(binary.LittleEndian.Uint32(b)))
	}
	return 0
}

func (r *nnetReader) float() float32 {
	if b := r.sized(); b != nil {
		return math.Float32frombits(binary.LittleEndian.Uint32(b))
	}
	return 0
}

// fv reads a float vector: "FV", count, floats.
func (r *nnetReader) fv() []float32 {
	if t := r.tok(); t != "FV" && r.err == nil {
		r.fail("expected FV, got %q", t)
	}
	n := r.int32()
	b := r.raw(4 * n)
	if b == nil {
		return nil
	}
	v := make([]float32, n)
	for k := range v {
		v[k] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*k:]))
	}
	return v
}

// sim reads a "SIM" int16 matrix and returns it dequantised by s.
func (r *nnetReader) sim(s float32) (rows, cols int, w []float32) {
	if t := r.tok(); t != "SIM" && r.err == nil {
		r.fail("expected SIM, got %q", t)
	}
	rows, cols = r.int32(), r.int32()
	if r.err != nil {
		return 0, 0, nil
	}
	if rows <= 0 || cols <= 0 || rows > len(r.d) || cols > len(r.d) {
		r.fail("bad matrix size %dx%d", rows, cols)
		return 0, 0, nil
	}
	b := r.raw(2 * rows * cols)
	if b == nil {
		return 0, 0, nil
	}
	w = make([]float32, rows*cols)
	for k := range w {
		w[k] = float32(int16(binary.LittleEndian.Uint16(b[2*k:]))) * s
	}
	return rows, cols, w
}

// parseNnet reads the text graph and the binary components and returns the
// nodes in graph order with their widths checked end to end.
func parseNnet(d []byte) ([]*node, error) {
	p := bytes.Index(d, []byte("<NumComponents>"))
	if p < 0 {
		return nil, errors.New("no <NumComponents>")
	}
	graph, err := parseGraph(string(d[:p]))
	if err != nil {
		return nil, err
	}
	if graph.inputDim != numMel {
		return nil, fmt.Errorf("input dim %d, want %d log-mel bins", graph.inputDim, numMel)
	}
	r := &nnetReader{d: d, i: p}
	r.tok()
	nc := r.int32()
	if r.err == nil && (nc < 0 || nc > len(d)) {
		r.fail("bad component count %d", nc)
	}
	comps := map[string]*node{}
	for range nc {
		if t := r.tok(); t != "<ComponentName>" && r.err == nil {
			r.fail("expected <ComponentName>, got %q", t)
		}
		name := r.tok()
		typ := strings.Trim(r.tok(), "<>")
		if r.err != nil {
			break
		}
		kind, ok := kindByType[typ]
		if !ok {
			return nil, fmt.Errorf("component %s: unknown type %s", name, typ)
		}
		c := &node{name: name, kind: kind}
		readComponent(r, c)
		if r.err != nil {
			return nil, fmt.Errorf("component %s: %w", name, r.err)
		}
		comps[name] = c
	}
	if t := r.tok(); t != "</Nnet3>" && r.err == nil {
		r.fail("expected </Nnet3>, got %q", t)
	}
	if r.err != nil {
		return nil, r.err
	}
	return wire(graph, comps)
}

// readComponent fills c from its tagged fields up to the closing tag.
func readComponent(r *nnetReader, c *node) {
	var s float32
	haveS := false
	for r.err == nil {
		t := r.tok()
		if strings.HasPrefix(t, "</") {
			return
		}
		switch t {
		case "<S>":
			s, haveS = r.float(), true
		case "<LinearParams>":
			if !haveS {
				r.fail("<LinearParams> without <S>")
				return
			}
			c.rows, c.cols, c.w = r.sim(s)
		case "<BiasParams>", "<Bias>":
			c.bias = r.fv()
		case "<Scales>":
			c.scales = r.fv()
		case "<ValueAvg>", "<DerivAvg>":
			r.fv()
		case "<ContextParam>":
			c.context = r.int32()
		case "<Dim>":
			c.dim = r.int32()
		case "<Count>":
			r.raw(9) // size byte + float64
		default:
			if r.err == nil {
				r.fail("unknown field %q", t)
			}
		}
	}
}

// graphNode is one line of the text config.
type graphNode struct {
	name, comp, input string
}

type graph struct {
	inputDim int
	nodes    []graphNode
}

// parseGraph reads input-node and component-node lines; other lines
// (output-node) are not needed.
func parseGraph(cfg string) (*graph, error) {
	g := &graph{inputDim: -1}
	for _, line := range strings.Split(cfg, "\n") {
		kv := map[string]string{}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		for _, f := range fields[1:] {
			if k, v, ok := strings.Cut(f, "="); ok {
				kv[k] = v
			}
		}
		switch {
		case fields[0] == "input-node":
			dim, err := strconv.Atoi(kv["dim"])
			if err != nil || dim <= 0 {
				return nil, fmt.Errorf("bad input-node: %q", line)
			}
			g.inputDim = dim
		case fields[0] == "component-node":
			g.nodes = append(g.nodes, graphNode{kv["name"], kv["component"], kv["input"]})
		}
	}
	if g.inputDim < 0 || len(g.nodes) == 0 {
		return nil, errors.New("config has no input-node or no component-node")
	}
	return g, nil
}

// wire orders components by the graph and checks that every layer's width
// fits the layer that feeds it.
func wire(g *graph, comps map[string]*node) ([]*node, error) {
	idx := map[string]int{"input": -1}
	width := map[string]int{"input": g.inputDim}
	net := make([]*node, 0, len(g.nodes))
	for _, gn := range g.nodes {
		src, ok := idx[gn.input]
		if !ok {
			return nil, fmt.Errorf("node %s: input %q is not an earlier node", gn.name, gn.input)
		}
		c := comps[gn.comp]
		if c == nil {
			return nil, fmt.Errorf("node %s: no component %q", gn.name, gn.comp)
		}
		if _, dup := idx[gn.name]; dup {
			return nil, fmt.Errorf("node %s: defined twice", gn.name)
		}
		nd := *c
		nd.name, nd.input = gn.name, src
		in := width[gn.input]
		out, err := checkWidths(&nd, in)
		if err != nil {
			return nil, fmt.Errorf("layer %s: %w", gn.name, err)
		}
		nd.dim = out
		idx[gn.name] = len(net)
		width[gn.name] = out
		net = append(net, &nd)
	}
	return net, nil
}

// heads returns the nodes no other node reads, in graph order: the network's
// outputs, whose concatenation is the posterior vector.
func heads(net []*node) []int {
	used := make([]bool, len(net))
	for _, n := range net {
		if n.input >= 0 {
			used[n.input] = true
		}
	}
	var h []int
	for i, u := range used {
		if !u {
			h = append(h, i)
		}
	}
	return h
}

// posteriors is the width of the network output.
func posteriors(net []*node) int {
	n := 0
	for _, i := range heads(net) {
		n += net[i].dim
	}
	return n
}

// checkWidths verifies c can take an in-wide input and returns its output width.
func checkWidths(c *node, in int) (int, error) {
	bad := func(what string, got int) (int, error) {
		return 0, fmt.Errorf("%s is %d, want %d", what, got, in)
	}
	switch c.kind {
	case kindAffine:
		if c.cols != in {
			return bad("input width", c.cols)
		}
		if c.bias != nil && len(c.bias) != c.rows {
			return 0, fmt.Errorf("bias has %d values, want %d", len(c.bias), c.rows)
		}
		return c.rows, nil
	case kindConv:
		if c.rows != in {
			return bad("channel count", c.rows)
		}
		if len(c.bias) != c.rows {
			return 0, fmt.Errorf("bias has %d values, want %d", len(c.bias), c.rows)
		}
		return c.rows, nil
	case kindBias:
		if len(c.bias) != in {
			return bad("bias length", len(c.bias))
		}
	case kindScale:
		if len(c.scales) != in {
			return bad("scales length", len(c.scales))
		}
	case kindSoftmax:
		if c.dim != in {
			return bad("dim", c.dim)
		}
	}
	return in, nil
}
