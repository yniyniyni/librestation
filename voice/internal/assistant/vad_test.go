package assistant

import (
	"math"
	"math/rand"
	"testing"
	"time"
)

const rate = 16000

func samples(d time.Duration) int { return int(d * rate / time.Second) }

// noise is white noise at about dbfs RMS.
func noise(r *rand.Rand, d time.Duration, dbfs float64) []int16 {
	amp := 32768 * math.Pow(10, dbfs/20)
	out := make([]int16, samples(d))
	for i := range out {
		out[i] = int16(r.NormFloat64() * amp)
	}
	return out
}

// tone is a 300 Hz sine at about dbfs RMS, standing in for speech.
func tone(d time.Duration, dbfs float64) []int16 {
	amp := 32768 * math.Pow(10, dbfs/20) * math.Sqrt2
	out := make([]int16, samples(d))
	for i := range out {
		out[i] = int16(amp * math.Sin(2*math.Pi*300*float64(i)/rate))
	}
	return out
}

const (
	quietDB  = -60
	speechDB = -25
	block    = 1000 // not a multiple of the 480-sample frame
)

// feed pushes pcm in odd-sized blocks and returns the first non-pending
// result with how much audio it took to get there.
func feed(e *endpointer, pcm []int16) (epResult, time.Duration) {
	for i := 0; i < len(pcm); i += block {
		j := min(i+block, len(pcm))
		if r := e.push(pcm[i:j]); r != epPending {
			return r, time.Duration(j) * time.Second / rate
		}
	}
	return epPending, time.Duration(len(pcm)) * time.Second / rate
}

func concat(parts ...[]int16) []int16 {
	var out []int16
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func near(got, want, tol time.Duration) bool { return got >= want-tol && got <= want+tol }

func TestVADStartEndAndLimits(t *testing.T) {
	r := rand.New(rand.NewSource(1))

	e := newEndpointer(startLimit)
	if res, at := feed(e, noise(r, 3900*time.Millisecond, quietDB)); res != epPending {
		t.Fatalf("silence: %v after %v, want pending before 4 s", res, at)
	}
	if res, _ := feed(e, noise(r, 200*time.Millisecond, quietDB)); res != epNoSpeech {
		t.Fatalf("silence 4.1 s: %v, want noSpeech", res)
	}

	e = newEndpointer(startLimit)
	pcm := concat(noise(r, 500*time.Millisecond, quietDB), tone(time.Second, speechDB), noise(r, 2*time.Second, quietDB))
	res, at := feed(e, pcm)
	if res != epDone {
		t.Fatalf("speech: %v, want done", res)
	}
	if !near(at, 2200*time.Millisecond, 60*time.Millisecond) {
		t.Errorf("ended after %v of audio, want 1 s speech + 0.7 s silence after 0.5 s", at)
	}
	if d := time.Duration(len(e.utterance())) * time.Second / rate; !near(d, 1300*time.Millisecond, 60*time.Millisecond) {
		t.Errorf("utterance %v, want about 1.3 s with pre-roll", d)
	}

	e = newEndpointer(startLimit)
	res, _ = feed(e, concat(noise(r, 500*time.Millisecond, quietDB), tone(12*time.Second, speechDB)))
	if res != epDone {
		t.Fatalf("long speech: %v, want done", res)
	}
	if d := time.Duration(len(e.utterance())) * time.Second / rate; !near(d, maxUtterance, 30*time.Millisecond) {
		t.Errorf("long utterance %v, want cut at %v", d, maxUtterance)
	}
}

func TestVADSpeechRightAway(t *testing.T) {
	// The command often follows the wake word without a pause.
	e := newEndpointer(startLimit)
	r := rand.New(rand.NewSource(2))
	res, _ := feed(e, concat(tone(time.Second, speechDB), noise(r, time.Second, quietDB)))
	if res != epDone {
		t.Fatalf("got %v, want done", res)
	}
	if d := time.Duration(len(e.utterance())) * time.Second / rate; !near(d, time.Second, 60*time.Millisecond) {
		t.Errorf("utterance %v, want about 1 s", d)
	}
}
