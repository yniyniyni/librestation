package ycmd

// Spotter is the streaming recogniser: fbank, network and decoder over one
// Model. It is not safe for concurrent use.
type Spotter struct {
	fb   fbank
	nn   *nnet
	dec  *decoder
	cmds []Command
	feat [numMel]float32
	mel  [numMel]float32 // the LDA front end's input
	lda  *ldaFront       // nil = log-mel goes straight to the network
}

// NewSpotter makes a recogniser over m. Several spotters may share m.
func NewSpotter(m *Model) *Spotter {
	s := &Spotter{
		nn:   newNnet(m.net),
		dec:  newDecoder(m.Commands, m.WSmooth, m.WMax, m.Rearm, posteriors(m.net)),
		cmds: m.Commands,
	}
	s.fb.init()
	if m.lda != nil {
		s.fb.initBins(mfccBins)
		s.lda = newLDAFront(m.lda, m.ldaCols, m.cmnWindow)
	}
	return s
}

// Dither adds noise of the given size to the samples, as the model's flags ask.
// An activation model takes digital silence around any word for its own word:
// it only ever heard microphones. The commands' results are checked without it.
func (s *Spotter) Dither(size float64) {
	s.fb.dither, s.fb.rng = size, 2463534242
}

// Push feeds 16 kHz samples at int16 scale, in any chunking, and calls fire
// (if not nil) for every hit, synchronously and in order. The frames that
// complete within one call go through the network together (up to
// maxBatch at a time), so a hit is reported before Push returns, as with
// one frame at a time, and in steady state Push does not allocate.
func (s *Spotter) Push(pcm []float32, fire func(cmd string, conf float64)) {
	for len(pcm) > 0 {
		n, ok := s.fb.fill(pcm)
		pcm = pcm[n:]
		if !ok {
			continue
		}
		if !s.feature() {
			continue
		}
		if s.nn.add(s.feat[:]) {
			s.decode(fire)
		}
	}
	s.decode(fire)
}

// feature computes the next network input from the full window; false while
// the front end still gathers context.
func (s *Spotter) feature() bool {
	if s.lda == nil {
		s.fb.frame(s.feat[:])
		return true
	}
	s.fb.frame(s.mel[:])
	return s.lda.push(s.mel[:], s.fb.energy, s.feat[:])
}

// decode runs the queued frames through the network and the decoder.
func (s *Spotter) decode(fire func(cmd string, conf float64)) {
	if s.nn.nin == 0 {
		return
	}
	post := s.nn.run()
	for len(post) > 0 {
		if c, conf := s.dec.push(post[:s.nn.nPost]); c >= 0 && fire != nil {
			fire(s.cmds[c].Name, conf)
		}
		post = post[s.nn.nPost:]
	}
}

// Reset forgets all audio, as if the spotter were new.
func (s *Spotter) Reset() {
	s.fb.reset()
	if s.lda != nil {
		s.lda.reset()
	}
	s.nn.reset()
	s.dec.reset()
}
