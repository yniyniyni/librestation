package ycmd

import "math"

// decoder is the chen14 posterior handling of the model's flags (Chen et
// al. 2014): each posterior is smoothed by the mean of the last wSmooth
// frames; a command's confidence is the geometric mean, over its labels, of
// the largest smoothed posterior in the last wMax frames.
//
// hit-at-peak: a command whose confidence has risen to its threshold fires
// on the first frame its confidence stops rising, with the peak value; one
// rise gives at most one hit. After a hit every window is zeroed
// (zeroing-confidence-buffer-after-hit) and nothing fires for rearm frames
// (interval-after-last-activation); a peak inside that interval is dropped.
type decoder struct {
	cmds                 []Command
	wSmooth, wMax, rearm int
	nPost                int

	raw    []float32 // wSmooth x nPost ring of posteriors
	smooth []float32 // wMax x nPost ring of smoothed posteriors
	rawAt  int       // next slot in raw
	smAt   int       // next slot in smooth

	conf   []float64 // per command, this frame
	prev   []float64 // per command, last frame
	rising []bool    // per command: above threshold and still rising
	quiet  int       // frames left before the next hit may fire
}

func newDecoder(cmds []Command, wSmooth, wMax, rearm, nPost int) *decoder {
	return &decoder{
		cmds: cmds, wSmooth: wSmooth, wMax: wMax, rearm: rearm, nPost: nPost,
		raw:    make([]float32, wSmooth*nPost),
		smooth: make([]float32, wMax*nPost),
		conf:   make([]float64, len(cmds)),
		prev:   make([]float64, len(cmds)),
		rising: make([]bool, len(cmds)),
	}
}

// zero empties every window, as after a hit.
func (d *decoder) zero() {
	clear(d.raw)
	clear(d.smooth)
	clear(d.prev)
	clear(d.rising)
}

func (d *decoder) reset() {
	d.zero()
	clear(d.conf)
	d.rawAt, d.smAt, d.quiet = 0, 0, 0
}

// push adds one posterior frame (nPost values) and returns the command
// that fires on it with its confidence, or -1.
func (d *decoder) push(post []float32) (int, float64) {
	copy(d.raw[d.rawAt*d.nPost:], post[:d.nPost])
	d.rawAt = (d.rawAt + 1) % d.wSmooth
	sm := d.smooth[d.smAt*d.nPost : (d.smAt+1)*d.nPost]
	d.smAt = (d.smAt + 1) % d.wMax
	clear(sm)
	for f := range d.wSmooth {
		for i, v := range d.raw[f*d.nPost : (f+1)*d.nPost] {
			sm[i] += v
		}
	}
	for i := range sm {
		sm[i] /= float32(d.wSmooth)
	}

	blocked := d.quiet > 0
	if blocked {
		d.quiet--
	}
	hit, hitConf := -1, 0.0
	for c, cmd := range d.cmds {
		lp := 0.0
		for _, l := range cmd.Labels {
			var m float32
			for f := range d.wMax {
				m = max(m, d.smooth[f*d.nPost+l])
			}
			lp += math.Log(math.Max(float64(m), 1e-12))
		}
		conf := math.Exp(lp / float64(len(cmd.Labels)))
		d.conf[c] = conf
		peak := d.rising[c] && conf <= d.prev[c]
		d.rising[c] = conf > d.prev[c] && conf >= cmd.Threshold
		if peak && !blocked && d.prev[c] > hitConf {
			hit, hitConf = c, d.prev[c]
		}
		d.prev[c] = conf
	}
	if hit >= 0 {
		d.zero()
		d.quiet = d.rearm
	}
	return hit, hitConf
}
