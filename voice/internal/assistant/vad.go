package assistant

import (
	"math"
	"slices"
	"time"
)

const (
	sampleRate = 16000
	frameLen   = 480 // 30 ms at 16 kHz
	frameDur   = 30 * time.Millisecond

	// speechAbove is how far over the noise floor a frame counts as speech.
	speechAbove = 9.0 // dB
	// startFrames speech frames in a row start the utterance.
	startFrames = 3
	// endQuiet below the threshold after the start ends it.
	endQuiet = 700 * time.Millisecond
	// preRoll is the audio kept from before the first speech frame.
	preRoll = 300 * time.Millisecond
	// maxUtterance bounds the utterance, pre-roll included.
	maxUtterance = 10 * time.Second

	// hangover is kept after the last speech frame for trailing consonants.
	hangover = 150 * time.Millisecond

	// The noise floor comes from the audio before the listen starts: a low
	// percentile of the frames in the last floorWindow, leaving out the most
	// recent floorSkip (the trigger word, or the reply's tail).
	floorWindow  = 3 * time.Second
	floorSkip    = time.Second
	floorMinUse  = 500 * time.Millisecond // less history than this: floorDefault
	floorPct     = 0.2
	floorMin     = -70.0 // dBFS; digital silence would make any hiss speech
	floorMax     = -30.0 // dBFS
	floorDefault = -60.0
)

type epResult int

const (
	epPending  epResult = iota
	epNoSpeech          // no start within the limit
	epDone              // the utterance ended or hit maxUtterance
	epStalled           // audio stopped arriving (the session's guard; never from push)
)

func (r epResult) String() string {
	return [...]string{"pending", "noSpeech", "done", "stalled"}[r]
}

// endpointer finds one utterance in a stream of 16 kHz audio, frame by
// frame. It is fed blocks of any size. Its noise floor is fixed for the
// listen: frames before the start may be the request itself.
type endpointer struct {
	startLimit int // frames to wait for the start
	floor      float64

	buf    []int16 // all audio taken in, whole frames only
	part   []int16 // the unfinished frame
	frames int

	run     int // speech frames in a row before the start
	started bool
	from    int // sample where the utterance begins (pre-roll included)
	lastEnd int // sample after the last speech frame
	quiet   int // quiet frames in a row after the start
	end     int // sample where the utterance ends, once done
	res     epResult
}

func newEndpointer(startLimit time.Duration, floor float64) *endpointer {
	return &endpointer{startLimit: int(startLimit / frameDur), floor: floor}
}

// push takes in pcm and says whether the utterance is over.
func (e *endpointer) push(pcm []int16) epResult {
	for len(pcm) > 0 && e.res == epPending {
		n := min(frameLen-len(e.part), len(pcm))
		e.part = append(e.part, pcm[:n]...)
		pcm = pcm[n:]
		if len(e.part) == frameLen {
			e.buf = append(e.buf, e.part...)
			e.part = e.part[:0]
			e.frame(level(e.buf[len(e.buf)-frameLen:]))
		}
	}
	return e.res
}

// utterance is the audio found, once push returned epDone.
func (e *endpointer) utterance() []int16 { return e.buf[e.from:e.end] }

func (e *endpointer) frame(db float64) {
	i := e.frames
	e.frames++
	speech := db >= e.floor+speechAbove
	if !e.started {
		if !speech {
			e.run = 0
		} else if e.run++; e.run >= startFrames {
			e.started = true
			first := (i - startFrames + 1) * frameLen
			e.from = max(0, first-samplesIn(preRoll))
			e.lastEnd = len(e.buf)
		}
		if !e.started && e.frames >= e.startLimit {
			e.res = epNoSpeech
		}
		return
	}
	if speech {
		e.quiet, e.lastEnd = 0, len(e.buf)
	} else {
		e.quiet++
	}
	switch {
	case len(e.buf)-e.from >= samplesIn(maxUtterance):
		e.end, e.res = e.from+samplesIn(maxUtterance), epDone
	case e.quiet >= int(endQuiet/frameDur):
		e.end, e.res = min(e.lastEnd+samplesIn(hangover), len(e.buf)), epDone
	}
}

// levels keeps the dBFS of the recent 30 ms frames, for the noise floor.
// It is fed every block, so push is cheap: one pass over the samples.
type levels struct {
	ring   [floorWindow / frameDur]float64
	frames int // frames written in all
	sum    float64
	n      int // samples in the unfinished frame
}

func (l *levels) push(pcm []int16) {
	for _, v := range pcm {
		l.sum += float64(v) * float64(v)
		if l.n++; l.n == frameLen {
			l.ring[l.frames%len(l.ring)] = 10*math.Log10(l.sum/frameLen+1e-9) - 90.3
			l.frames++
			l.sum, l.n = 0, 0
		}
	}
}

// floor is the noise floor for a listen starting now.
func (l *levels) floor() float64 {
	have := min(l.frames, len(l.ring))
	use := have - int(floorSkip/frameDur)
	if use < int(floorMinUse/frameDur) {
		return floorDefault
	}
	db := make([]float64, 0, use)
	for i := l.frames - have; i < l.frames-have+use; i++ {
		db = append(db, l.ring[i%len(l.ring)])
	}
	slices.Sort(db)
	return min(max(db[int(floorPct*float64(use))], floorMin), floorMax)
}

// seedLen is the raw audio kept from before a listen opens: the wake fires
// up to ~400 ms after the trigger word, and the request may start at once.
const seedLen = sampleRate / 2

// recent keeps the last seedLen samples. push is a copy, no allocation.
type recent struct {
	ring [seedLen]int16
	n    int // samples written in all
}

func (r *recent) push(pcm []int16) {
	if len(pcm) > len(r.ring) {
		r.n += len(pcm) - len(r.ring)
		pcm = pcm[len(pcm)-len(r.ring):]
	}
	i := r.n % len(r.ring)
	c := copy(r.ring[i:], pcm)
	copy(r.ring[:], pcm[c:])
	r.n += len(pcm)
}

// appendTo appends the kept samples, oldest first.
func (r *recent) appendTo(dst []int16) []int16 {
	have := min(r.n, len(r.ring))
	i := (r.n - have) % len(r.ring)
	if i+have <= len(r.ring) {
		return append(dst, r.ring[i:i+have]...)
	}
	dst = append(dst, r.ring[i:]...)
	return append(dst, r.ring[:have-(len(r.ring)-i)]...)
}

func samplesIn(d time.Duration) int { return int(d * sampleRate / time.Second) }

// level is a frame's RMS in dBFS.
func level(pcm []int16) float64 {
	var e float64
	for _, s := range pcm {
		e += float64(s) * float64(s)
	}
	return 10*math.Log10(e/float64(max(len(pcm), 1))+1e-9) - 90.3
}
