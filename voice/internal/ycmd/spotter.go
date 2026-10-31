package ycmd

// Spotter is the streaming recogniser: fbank, network and decoder over one
// Model. It is not safe for concurrent use.
type Spotter struct {
	fb   fbank
	nn   *nnet
	dec  *decoder
	cmds []Command
	feat [numMel]float32
}

// NewSpotter makes a recogniser over m. Several spotters may share m.
func NewSpotter(m *Model) *Spotter {
	s := &Spotter{
		nn:   newNnet(m.net),
		dec:  newDecoder(m.Commands, m.WSmooth, m.WMax, m.Rearm, posteriors(m.net)),
		cmds: m.Commands,
	}
	s.fb.init()
	return s
}

// Push feeds 16 kHz samples at int16 scale, in any chunking, and calls fire
// (if not nil) for every hit, synchronously and in order. In steady state it
// does not allocate.
func (s *Spotter) Push(pcm []float32, fire func(cmd string, conf float64)) {
	for len(pcm) > 0 {
		n, ok := s.fb.fill(pcm)
		pcm = pcm[n:]
		if !ok {
			continue
		}
		s.fb.frame(s.feat[:])
		post, ok := s.nn.step(s.feat[:])
		if !ok {
			continue
		}
		if c, conf := s.dec.push(post); c >= 0 && fire != nil {
			fire(s.cmds[c].Name, conf)
		}
	}
}

// Reset forgets all audio, as if the spotter were new.
func (s *Spotter) Reset() {
	s.fb.reset()
	s.nn.reset()
	s.dec.reset()
}
