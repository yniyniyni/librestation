package ycmd

import "math"

// maxBatch is how many feature frames the network takes in one pass. The
// layers are causal, so running frames together gives the same posteriors
// as one at a time, but each weight loaded serves every frame of the pass:
// on the speaker's in-order Cortex-A53 the loops are bound by their loads,
// and the 1.3 MB of weights do not stay in cache. The model's own flags use
// am-batch-size 6.
const maxBatch = 6

// nnet runs a Model's graph over batches of feature frames. Every buffer is
// allocated up front; add and run do not allocate.
type nnet struct {
	nodes []*node
	out   [][]float32 // each node's output, frame-major, up to maxBatch frames
	cnt   []int       // frames in each out
	gemvs []*gemv     // per node; nil unless the node is an affine
	convs []*conv     // per node; nil unless the node is a conv
	heads []int       // output nodes, concatenated into post
	nPost int
	in    []float32 // queued input frames, maxBatch x numMel
	nin   int
	post  []float32 // maxBatch x nPost
}

// lanes is the width of an affine layer's register blocks: it works on
// panels of lanes rows, with one sum per row and frame in registers. Eight
// rows by two frames (16 sums, 10 loads per 16 FMAs) measured best on the
// speaker; more sums spill.
const lanes = 8

// gemv is an affine layer with its rows interleaved in panels: panel p
// holds rows p*lanes.. column by column, so the inner loop reads one
// sequential stream. Rows past the end of the matrix are zero.
type gemv struct {
	rows, cols int
	w          []float32 // panels x cols x lanes
	bias       []float32 // rows, zero if the layer has none
}

// groupLanes is how many channels a conv filters at once: its loop needs
// a weight and two inputs per pair of sums, so a wider group spills.
const groupLanes = 4

// conv is the streaming state of a depthwise FIR, kept in groups of
// groupLanes channels. Each group is one block of memory: its weights,
// taps x groupLanes, then its history, slots x groupLanes, both interleaved
// like a gemv panel (lanes past the last channel are zero). The history is a line that new
// frames are appended to; when it is full the last taps-1 frames move back
// to the start. So a frame's window is always contiguous, oldest first, a
// pass walks memory forward, and new inputs land next to the window.
type conv struct {
	taps, ch int
	slots    int       // history length in frames
	block    []float32 // groups x (taps+slots) x groupLanes
	end      int       // history frames in use; the newest is end-1
	skip     int       // initial output frames still to drop
}

// convSpare is how many frames a conv history holds beyond a full batch's
// windows, i.e. how many frames pass between the moves back.
const convSpare = 16

func newNnet(net []*node) *nnet {
	n := &nnet{
		nodes: net,
		out:   make([][]float32, len(net)),
		cnt:   make([]int, len(net)),
		gemvs: make([]*gemv, len(net)),
		convs: make([]*conv, len(net)),
		heads: heads(net),
		nPost: posteriors(net),
		in:    make([]float32, maxBatch*numMel),
	}
	n.post = make([]float32, maxBatch*n.nPost)
	for i, nd := range net {
		n.out[i] = make([]float32, maxBatch*nd.dim)
		switch nd.kind {
		case kindAffine:
			n.gemvs[i] = newGemv(nd)
		case kindConv:
			n.convs[i] = newConv(nd)
		}
	}
	n.reset()
	return n
}

func newConv(nd *node) *conv {
	c := &conv{taps: nd.cols, ch: nd.rows, slots: nd.cols + maxBatch - 1 + convSpare}
	groups := (c.ch + groupLanes - 1) / groupLanes
	c.block = make([]float32, groups*(c.taps+c.slots)*groupLanes)
	for ch := range c.ch {
		w := c.group(ch / groupLanes)
		for k := range c.taps {
			w[k*groupLanes+ch%groupLanes] = nd.w[ch*c.taps+k]
		}
	}
	return c
}

// group returns group g's block: weights, then history.
func (c *conv) group(g int) []float32 {
	n := (c.taps + c.slots) * groupLanes
	return c.block[g*n : (g+1)*n]
}

// reset empties the history: taps-1 frames of silence.
func (c *conv) reset(skip int) {
	for g := 0; g*groupLanes < c.ch; g++ {
		clear(c.group(g)[c.taps*groupLanes:])
	}
	c.end = c.taps - 1
	c.skip = skip
}

func newGemv(nd *node) *gemv {
	panels := (nd.rows + lanes - 1) / lanes
	g := &gemv{rows: nd.rows, cols: nd.cols, w: make([]float32, panels*nd.cols*lanes), bias: nd.bias}
	for r := range nd.rows {
		p, j := r/lanes, r%lanes
		for k := range nd.cols {
			g.w[(p*nd.cols+k)*lanes+j] = nd.w[r*nd.cols+k]
		}
	}
	if g.bias == nil {
		g.bias = make([]float32, nd.rows)
	}
	return g
}

// reset clears the conv history and the queue, as at the start of a stream.
func (n *nnet) reset() {
	n.nin = 0
	for i, c := range n.convs {
		if c != nil {
			c.reset(n.nodes[i].context)
		}
	}
}

// add queues one numMel-wide feature frame and reports whether the batch
// is full; run must be called before adding to a full batch.
func (n *nnet) add(feat []float32) bool {
	copy(n.in[n.nin*numMel:(n.nin+1)*numMel], feat)
	n.nin++
	return n.nin == maxBatch
}

// run passes the queued frames through the network and empties the queue.
// It returns the posteriors of the frames that produce output, nPost
// values per frame in order; while a conv is still dropping its first
// outputs (the model's lookahead) a frame produces none. The slice is
// reused by the next run.
func (n *nnet) run() []float32 {
	nin := n.nin
	n.nin = 0
	for i, nd := range n.nodes {
		x, nx, w := n.in, nin, numMel
		if nd.input >= 0 {
			x, nx, w = n.out[nd.input], n.cnt[nd.input], n.nodes[nd.input].dim
		}
		x = x[:nx*w]
		y := n.out[i][:nx*nd.dim]
		ny := nx
		switch nd.kind {
		case kindBias:
			for f := range nx {
				xf, yf := x[f*nd.dim:(f+1)*nd.dim], y[f*nd.dim:(f+1)*nd.dim]
				for j, v := range nd.bias {
					yf[j] = xf[j] + v
				}
			}
		case kindScale:
			for f := range nx {
				xf, yf := x[f*nd.dim:(f+1)*nd.dim], y[f*nd.dim:(f+1)*nd.dim]
				for j, v := range nd.scales {
					yf[j] = xf[j] * v
				}
			}
		case kindAffine:
			n.gemvs[i].run(x, y, nx)
		case kindConv:
			ny = n.convs[i].run(x, n.out[i], nx, nd.bias)
		case kindHardswish:
			for j, v := range x {
				y[j] = v * min(max(v+3, 0), 6) / 6
			}
		case kindSoftmax:
			for f := range nx {
				softmax(x[f*nd.dim:(f+1)*nd.dim], y[f*nd.dim:(f+1)*nd.dim])
			}
		}
		n.cnt[i] = ny
	}
	nf := n.cnt[n.heads[0]]
	p := n.post[:0]
	for f := range nf {
		for _, h := range n.heads {
			d := n.nodes[h].dim
			p = append(p, n.out[h][f*d:(f+1)*d]...)
		}
	}
	return p
}

// run writes w*x + bias to y for nf frames, two at a time so that each
// weight read from memory serves both.
func (g *gemv) run(x, y []float32, nf int) {
	f := 0
	for ; f+2 <= nf; f += 2 {
		g.run2(x[f*g.cols:(f+1)*g.cols], x[(f+1)*g.cols:(f+2)*g.cols], y[f*g.rows:(f+1)*g.rows], y[(f+1)*g.rows:(f+2)*g.rows])
	}
	if f < nf {
		g.run1(x[f*g.cols:(f+1)*g.cols], y[f*g.rows:(f+1)*g.rows])
	}
}

func (g *gemv) run1(x, y []float32) {
	x = x[:g.cols]
	stride := g.cols * lanes
	for r := 0; r < g.rows; r += lanes {
		p := g.w[r*g.cols : r*g.cols+stride]
		var a0, a1, a2, a3, a4, a5, a6, a7 float32
		for k, v := range x {
			q := p[k*lanes : k*lanes+lanes : k*lanes+lanes]
			a0 += q[0] * v
			a1 += q[1] * v
			a2 += q[2] * v
			a3 += q[3] * v
			a4 += q[4] * v
			a5 += q[5] * v
			a6 += q[6] * v
			a7 += q[7] * v
		}
		a := [lanes]float32{a0, a1, a2, a3, a4, a5, a6, a7}
		put(y[r:], g.bias[r:], a[:])
	}
}

func (g *gemv) run2(x0, x1, y0, y1 []float32) {
	x0 = x0[:g.cols]
	x1 = x1[:len(x0)]
	stride := g.cols * lanes
	for r := 0; r < g.rows; r += lanes {
		p := g.w[r*g.cols : r*g.cols+stride]
		var a0, a1, a2, a3, a4, a5, a6, a7, b0, b1, b2, b3, b4, b5, b6, b7 float32
		for k, v := range x0 {
			u := x1[k]
			q := p[k*lanes : k*lanes+lanes : k*lanes+lanes]
			a0 += q[0] * v
			a1 += q[1] * v
			a2 += q[2] * v
			a3 += q[3] * v
			a4 += q[4] * v
			a5 += q[5] * v
			a6 += q[6] * v
			a7 += q[7] * v
			b0 += q[0] * u
			b1 += q[1] * u
			b2 += q[2] * u
			b3 += q[3] * u
			b4 += q[4] * u
			b5 += q[5] * u
			b6 += q[6] * u
			b7 += q[7] * u
		}
		a := [lanes]float32{a0, a1, a2, a3, a4, a5, a6, a7}
		b := [lanes]float32{b0, b1, b2, b3, b4, b5, b6, b7}
		put(y0[r:], g.bias[r:], a[:])
		put(y1[r:], g.bias[r:], b[:])
	}
}

// put stores the sums of one block plus their bias, as many as y has room
// for: the last block of a layer may be short.
func put(y, bias, a []float32) {
	y = y[:min(len(y), len(a))]
	bias = bias[:len(y)]
	for j := range y {
		y[j] = a[j] + bias[j]
	}
}

// run pushes nf frames of x into the history and writes the filtered
// frames to y; tap 0 multiplies the oldest frame. Frames still being
// dropped give no output; it returns how many frames it wrote.
func (c *conv) run(x, y []float32, nf int, bias []float32) int {
	ch, t := c.ch, c.taps
	move := c.end+nf > c.slots
	drop := min(c.skip, nf)
	c.skip -= drop
	for g := 0; g*groupLanes < ch; g++ {
		blk := c.group(g)
		w, h := blk[:t*groupLanes], blk[t*groupLanes:]
		if move {
			copy(h, h[(c.end-(t-1))*groupLanes:c.end*groupLanes])
		}
		end := c.end
		if move {
			end = t - 1
		}
		j := g * groupLanes
		n := min(groupLanes, ch-j)
		for f := range nf {
			copy(h[(end+f)*groupLanes:(end+f)*groupLanes+n], x[f*ch+j:f*ch+j+n])
		}
		bias := bias[j : j+n]
		// Frame f's window ends at history frame end+f.
		f := drop
		for ; f+2 <= nf; f += 2 {
			a, b := fir2(w, h[(end+f-(t-1))*groupLanes:])
			put(y[(f-drop)*ch+j:(f-drop)*ch+j+n], bias, a[:])
			put(y[(f+1-drop)*ch+j:(f+1-drop)*ch+j+n], bias, b[:])
		}
		if f < nf {
			a := fir1(w, h[(end+f-(t-1))*groupLanes:])
			put(y[(f-drop)*ch+j:(f-drop)*ch+j+n], bias, a[:])
		}
	}
	if move {
		c.end = t - 1
	}
	c.end += nf
	return nf - drop
}

// fir1 filters one group for one frame: w is taps x groupLanes, h the
// frame's inputs from the oldest on. The loops walk w and h by re-slicing,
// which keeps their loads paired and their bounds checks to one per tap.
func fir1(w, h []float32) [groupLanes]float32 {
	var a0, a1, a2, a3 float32
	for len(w) >= groupLanes && len(h) >= groupLanes {
		q, v := w[:groupLanes:groupLanes], h[:groupLanes:groupLanes]
		a0 += q[0] * v[0]
		a1 += q[1] * v[1]
		a2 += q[2] * v[2]
		a3 += q[3] * v[3]
		w, h = w[groupLanes:], h[groupLanes:]
	}
	return [groupLanes]float32{a0, a1, a2, a3}
}

// fir2 is fir1 for two consecutive frames: the second frame's window is
// the first one's shifted by one input.
func fir2(w, h []float32) (a, b [groupLanes]float32) {
	var a0, a1, a2, a3, b0, b1, b2, b3 float32
	for len(w) >= groupLanes && len(h) >= 2*groupLanes {
		q, v := w[:groupLanes:groupLanes], h[:2*groupLanes:2*groupLanes]
		a0 += q[0] * v[0]
		a1 += q[1] * v[1]
		a2 += q[2] * v[2]
		a3 += q[3] * v[3]
		b0 += q[0] * v[4]
		b1 += q[1] * v[5]
		b2 += q[2] * v[6]
		b3 += q[3] * v[7]
		w, h = w[groupLanes:], h[groupLanes:]
	}
	return [groupLanes]float32{a0, a1, a2, a3}, [groupLanes]float32{b0, b1, b2, b3}
}

func softmax(x, y []float32) {
	m := x[0]
	for _, v := range x {
		m = max(m, v)
	}
	var s float64
	for i, v := range x {
		e := math.Exp(float64(v - m))
		y[i] = float32(e)
		s += e
	}
	for i := range y {
		y[i] /= float32(s)
	}
}
