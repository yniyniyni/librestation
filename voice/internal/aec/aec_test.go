package aec

import (
	"math"
	"math/rand"
	"testing"
)

const (
	rate   = 16000
	block  = 64
	parts  = 64
	frame  = 320 // what the frontend hands us: 20 ms
	secs   = 6
	nTotal = secs * rate
)

// music returns noise-like "music": white noise through a resonant
// low-pass, scaled to the given RMS.
func music(rng *rand.Rand, n int, rms float64) []float64 {
	x := make([]float64, n)
	var y1, y2 float64
	for i := range x {
		w := rng.NormFloat64()
		y := 1.6*y1 - 0.7*y2 + w // 2-pole resonator, coloured spectrum
		y2, y1 = y1, y
		x[i] = y
	}
	return scaleTo(x, rms)
}

func scaleTo(x []float64, rms float64) []float64 {
	var s float64
	for _, v := range x {
		s += v * v
	}
	g := rms / math.Sqrt(s/float64(len(x)))
	for i := range x {
		x[i] *= g
	}
	return x
}

// echoOf convolves ref with a 30 ms bulk delay followed by a random
// exponentially decaying 100 ms impulse response, scaled to echoRMS.
func echoOf(rng *rand.Rand, ref []float64, echoRMS float64) []float64 {
	delay := 30 * rate / 1000
	irLen := 100 * rate / 1000
	ir := make([]float64, irLen)
	for i := range ir {
		ir[i] = rng.NormFloat64() * math.Exp(-float64(i)/float64(irLen/5))
	}
	out := make([]float64, len(ref))
	for i := range out {
		var s float64
		for j, h := range ir {
			k := i - delay - j
			if k < 0 {
				break
			}
			s += h * ref[k]
		}
		out[i] = s
	}
	return scaleTo(out, echoRMS)
}

type scenario struct {
	ref, mic, tone []float64
}

// makeScenario builds the brief's setup. Scale is int16-like: ref at
// refRMS, echo at echoRMS, tiny sensor noise 50 dB below the echo, and
// (if toneDB is not nil) a 1 kHz near-end tone during 4–5 s whose RMS is
// *toneDB relative to the echo RMS.
func makeScenario(seed int64, refRMS, echoRMS float64, toneDB *float64) scenario {
	rng := rand.New(rand.NewSource(seed))
	ref := music(rng, nTotal, refRMS)
	echo := echoOf(rng, ref, echoRMS)
	noise := echoRMS * math.Pow(10, -50.0/20)
	mic := make([]float64, nTotal)
	tone := make([]float64, nTotal)
	if toneDB != nil {
		amp := echoRMS * math.Sqrt2 * math.Pow(10, *toneDB/20)
		for i := 4 * rate; i < 5*rate; i++ {
			tone[i] = amp * math.Sin(2*math.Pi*1000*float64(i)/rate)
		}
	}
	for i := range mic {
		mic[i] = echo[i] + tone[i] + noise*rng.NormFloat64()
	}
	return scenario{ref: ref, mic: mic, tone: tone}
}

func run(t testing.TB, c *Canceller, sc scenario) []float64 {
	out := make([]float64, 0, nTotal)
	m := make([]float32, frame)
	r := make([]float32, frame)
	for off := 0; off+frame <= nTotal; off += frame {
		for i := 0; i < frame; i++ {
			m[i] = float32(sc.mic[off+i])
			r[i] = float32(sc.ref[off+i])
		}
		o := c.Process(m, r)
		if len(o) != frame {
			t.Fatalf("Process returned %d samples, want %d", len(o), frame)
		}
		for _, v := range o {
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				t.Fatalf("non-finite output at %d", off)
			}
			out = append(out, float64(v))
		}
	}
	return out
}

func energy(x []float64, from, to int) float64 {
	var s float64
	for _, v := range x[from:to] {
		s += v * v
	}
	return s
}

func erleDB(mic, out []float64, from, to int) float64 {
	return 10 * math.Log10(energy(mic, from, to)/energy(out, from, to))
}

// toneAmp measures the 1 kHz component's amplitude in x[from:to] (lock-in).
func toneAmp(x []float64, from, to int) float64 {
	var c, s float64
	for i := from; i < to; i++ {
		w := 2 * math.Pi * 1000 * float64(i) / rate
		c += x[i] * math.Cos(w)
		s += x[i] * math.Sin(w)
	}
	return 2 * math.Hypot(c, s) / float64(to-from)
}

func TestEchoReduction(t *testing.T) {
	for _, tc := range []struct {
		name            string
		refRMS, echoRMS float64
	}{
		{"loud", 3000, 1000},
		{"quiet mic", 3000, 40}, // raw mic before gain is tiny
		{"quiet ref", 50, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := makeScenario(1, tc.refRMS, tc.echoRMS, nil)
			out := run(t, New(block, parts), sc)
			erle := erleDB(sc.mic, out, 3*rate, nTotal)
			t.Logf("ERLE after 3 s: %.1f dB", erle)
			if erle < 15 {
				t.Fatalf("ERLE %.1f dB < 15 dB", erle)
			}
		})
	}
}

func TestNearEndPreserved(t *testing.T) {
	for _, tc := range []struct {
		name   string
		toneDB float64 // near-end level relative to echo
	}{
		{"equal to echo", 0},
		{"20 dB over echo", 20}, // exercises the double-talk guard
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := makeScenario(2, 3000, 1000, &tc.toneDB)
			out := run(t, New(block, parts), sc)
			in := toneAmp(sc.tone, 4*rate, 5*rate)
			got := toneAmp(out, 4*rate, 5*rate)
			db := 20 * math.Log10(got/in)
			t.Logf("near-end tone: in %.1f out %.1f (%.2f dB)", in, got, db)
			if math.Abs(db) > 3 {
				t.Fatalf("near-end tone changed by %.2f dB (> 3 dB)", db)
			}
			// Double talk must not wreck the filter: echo still cancelled after.
			erle := erleDB(sc.mic, out, 5*rate+rate/2, nTotal)
			t.Logf("ERLE 5.5-6 s after double talk: %.1f dB", erle)
			if erle < 15 {
				t.Fatalf("ERLE after double talk %.1f dB < 15 dB", erle)
			}
		})
	}
}

func TestSilentReferencePassesThrough(t *testing.T) {
	c := New(block, parts)
	rng := rand.New(rand.NewSource(3))
	m := make([]float32, frame)
	r := make([]float32, frame)
	for f := 0; f < 200; f++ {
		for i := range m {
			m[i] = float32(300 * rng.NormFloat64())
		}
		o := c.Process(m, r)
		for i, v := range o {
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				t.Fatalf("frame %d: non-finite output", f)
			}
			if d := math.Abs(float64(v - m[i])); d > 1e-3 {
				t.Fatalf("frame %d sample %d: out %v != mic %v", f, i, v, m[i])
			}
		}
	}
}

// Music pauses (reference goes silent) and resumes: no blow-up, and
// cancellation is back quickly.
func TestReferencePauseResume(t *testing.T) {
	sc := makeScenario(4, 3000, 1000, nil)
	for i := 3 * rate; i < 4*rate; i++ {
		sc.ref[i] = 0
	}
	// Recompute the mic consistently: echo of the gated reference.
	rng := rand.New(rand.NewSource(4))
	_ = music(rng, nTotal, 3000) // advance rng identically
	echo := echoOf(rng, sc.ref, 1000)
	for i := range sc.mic {
		sc.mic[i] = echo[i] + 3*rng.NormFloat64()
	}
	out := run(t, New(block, parts), sc)
	erle := erleDB(sc.mic, out, 5*rate, nTotal)
	t.Logf("ERLE 5-6 s after pause: %.1f dB", erle)
	if erle < 15 {
		t.Fatalf("ERLE after pause %.1f dB < 15 dB", erle)
	}
}

func TestProcessAllocationFree(t *testing.T) {
	c := New(block, parts)
	sc := makeScenario(5, 3000, 1000, nil)
	m := make([]float32, frame)
	r := make([]float32, frame)
	for i := range m {
		m[i], r[i] = float32(sc.mic[i]), float32(sc.ref[i])
	}
	c.Process(m, r) // warm up (sizes output buffer)
	if a := testing.AllocsPerRun(50, func() { c.Process(m, r) }); a != 0 {
		t.Fatalf("Process allocates %.1f times per call", a)
	}
}

func TestProcessRejectsBadLength(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for length not a multiple of block")
		}
	}()
	New(block, parts).Process(make([]float32, 100), make([]float32, 100))
}

func BenchmarkProcess320(b *testing.B) {
	c := New(block, parts)
	sc := makeScenario(6, 3000, 1000, nil)
	m := make([]float32, frame)
	r := make([]float32, frame)
	for i := range m {
		m[i], r[i] = float32(sc.mic[i]), float32(sc.ref[i])
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Process(m, r)
	}
}
