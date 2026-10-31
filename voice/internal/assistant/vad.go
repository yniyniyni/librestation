package assistant

import (
	"math"
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

	// floorAlpha is the EMA weight of a quiet frame in the noise floor.
	floorAlpha = 0.1
	// The floor starts at the first frame's level clamped to this range: the
	// command often follows the wake word without a pause, so the first frame
	// may already be speech; digital silence would make any hiss speech.
	floorMax = -45.0 // dBFS
	floorMin = -70.0 // dBFS
)

type epResult int

const (
	epPending  epResult = iota
	epNoSpeech          // no start within the limit
	epDone              // the utterance ended or hit maxUtterance
)

func (r epResult) String() string {
	return [...]string{"pending", "noSpeech", "done"}[r]
}

// endpointer finds one utterance in a stream of 16 kHz audio, frame by
// frame. It is fed blocks of any size.
type endpointer struct {
	startLimit int // frames to wait for the start

	buf    []int16 // all audio taken in, whole frames only
	part   []int16 // the unfinished frame
	frames int

	floor   float64
	haveFl  bool
	run     int // speech frames in a row before the start
	started bool
	from    int // sample where the utterance begins (pre-roll included)
	lastEnd int // sample after the last speech frame
	quiet   int // quiet frames in a row after the start
	end     int // sample where the utterance ends, once done
	res     epResult
}

func newEndpointer(startLimit time.Duration) *endpointer {
	return &endpointer{startLimit: int(startLimit / frameDur)}
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
	if !e.haveFl {
		e.floor, e.haveFl = min(max(db, floorMin), floorMax), true
	}
	speech := db >= e.floor+speechAbove
	if !e.started {
		if !speech {
			e.run = 0
			e.floor = max(e.floor+floorAlpha*(db-e.floor), floorMin)
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
		e.end, e.res = e.lastEnd, epDone
	}
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
