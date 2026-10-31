package ycmd

import "math"

// nnet runs a Model's graph one feature frame at a time. Every buffer is
// allocated up front; step does not allocate.
type nnet struct {
	nodes []*node
	out   [][]float32 // each node's output for the current frame
	convs []*conv     // per node; nil unless the node is a conv
	heads []int       // output nodes, concatenated into post
	post  []float32
}

// conv is the streaming state of a depthwise FIR: the last taps input
// frames in a ring, with weights stored tap-major so both are walked in
// memory order.
type conv struct {
	taps, ch int
	wt       []float32 // taps x channels: wt[k*ch+c] = w[c*taps+k]
	hist     []float32 // taps x channels ring
	pos      int       // ring slot of the newest frame
	skip     int       // initial output frames still to drop
}

func newNnet(net []*node) *nnet {
	n := &nnet{
		nodes: net,
		out:   make([][]float32, len(net)),
		convs: make([]*conv, len(net)),
		heads: heads(net),
		post:  make([]float32, posteriors(net)),
	}
	for i, nd := range net {
		n.out[i] = make([]float32, nd.dim)
		if nd.kind != kindConv {
			continue
		}
		c := &conv{taps: nd.cols, ch: nd.rows, wt: make([]float32, len(nd.w)), hist: make([]float32, len(nd.w))}
		for ch := range c.ch {
			for k := range c.taps {
				c.wt[k*c.ch+ch] = nd.w[ch*c.taps+k]
			}
		}
		n.convs[i] = c
	}
	n.reset()
	return n
}

// reset clears the conv history, as at the start of a stream.
func (n *nnet) reset() {
	for i, c := range n.convs {
		if c != nil {
			clear(c.hist)
			c.pos = c.taps - 1
			c.skip = n.nodes[i].context
		}
	}
}

// step consumes one numMel-wide feature frame. It returns the posteriors,
// or false while a conv is still dropping its first outputs (the model's
// lookahead). The slice is reused by the next step.
func (n *nnet) step(feat []float32) ([]float32, bool) {
	for i, nd := range n.nodes {
		x := feat
		if nd.input >= 0 {
			x = n.out[nd.input]
		}
		y := n.out[i]
		switch nd.kind {
		case kindBias:
			for j, v := range x {
				y[j] = v + nd.bias[j]
			}
		case kindScale:
			for j, v := range x {
				y[j] = v * nd.scales[j]
			}
		case kindAffine:
			for r := range nd.rows {
				row := nd.w[r*nd.cols : (r+1)*nd.cols]
				var acc float32
				for k, w := range row {
					acc += w * x[k]
				}
				if nd.bias != nil {
					acc += nd.bias[r]
				}
				y[r] = acc
			}
		case kindConv:
			if !n.convs[i].run(x, y, nd.bias) {
				return nil, false
			}
		case kindHardswish:
			for j, v := range x {
				y[j] = v * min(max(v+3, 0), 6) / 6
			}
		case kindSoftmax:
			softmax(x, y)
		}
	}
	p := n.post[:0]
	for _, h := range n.heads {
		p = append(p, n.out[h]...)
	}
	return p, true
}

// run pushes x into the history and writes the filtered frame to y; tap 0
// multiplies the oldest frame. It reports false while dropping outputs.
func (c *conv) run(x, y, bias []float32) bool {
	c.pos = (c.pos + 1) % c.taps
	copy(c.hist[c.pos*c.ch:(c.pos+1)*c.ch], x)
	if c.skip > 0 {
		c.skip--
		return false
	}
	clear(y)
	for k := range c.taps {
		slot := (c.pos + 1 + k) % c.taps // oldest first
		w := c.wt[k*c.ch : (k+1)*c.ch]
		h := c.hist[slot*c.ch : (slot+1)*c.ch]
		for j := range y {
			y[j] += w[j] * h[j]
		}
	}
	for j := range y {
		y[j] += bias[j]
	}
	return true
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
