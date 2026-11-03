package assistant

import "math"

const (
	earconRate = 24000
	// earconLead is silence for the output to wake up in, so the first note is
	// not clipped.
	earconLead = 0.05
	earconPeak = 0.7
	// earconMin is how long the sound is with the silence after it. Android does
	// not start an output until its buffer (8192 frames, 341 ms) is full, and a
	// shorter sound would wait there for the reply.
	earconMin = 0.4
)

// earconNotes are the two rising notes heard after the trigger word: frequency
// in Hz and length in seconds.
var earconNotes = [...]struct{ hz, secs float64 }{{659.25, 0.08}, {987.77, 0.15}}

// Earcon is the short sound that says the speaker heard its name. It is made
// here: one soft sine per note, so it needs no file and no stock sound.
func Earcon() ([]int16, int) {
	pcm := make([]int16, int(earconLead*earconRate), earconRate/2)
	for _, n := range earconNotes {
		total := int(n.secs * earconRate)
		for i := range total {
			t := float64(i) / earconRate
			// Quick in, longer out: a click-free soft bell.
			env := min(t/0.008, 1) * math.Pow(1-float64(i)/float64(total), 0.8)
			pcm = append(pcm, int16(earconPeak*32767*env*math.Sin(2*math.Pi*n.hz*t)))
		}
	}
	for len(pcm) < earconMin*earconRate {
		pcm = append(pcm, 0)
	}
	return pcm, earconRate
}
