package dsp

import (
	"math"
	"math/rand"
	"testing"
)

func rms(x []float32) float64 {
	var s float64
	for _, v := range x {
		s += float64(v) * float64(v)
	}
	return math.Sqrt(s / float64(len(x)))
}

func sine(freq float64, n int) []float32 {
	x := make([]float32, n)
	for i := range x {
		x[i] = float32(10000 * math.Sin(2*math.Pi*freq*float64(i)/48000))
	}
	return x
}

func TestFFTRoundTrip(t *testing.T) {
	for _, n := range []int{2, 8, 128, 512} {
		rng := rand.New(rand.NewSource(1))
		re := make([]float64, n)
		im := make([]float64, n)
		or, oi := make([]float64, n), make([]float64, n)
		for i := range re {
			re[i], im[i] = rng.NormFloat64(), rng.NormFloat64()
			or[i], oi[i] = re[i], im[i]
		}
		FFT(re, im)
		IFFT(re, im)
		for i := range re {
			if math.Abs(re[i]-or[i]) > 1e-9 || math.Abs(im[i]-oi[i]) > 1e-9 {
				t.Fatalf("n=%d i=%d mismatch", n, i)
			}
		}
	}
}

func TestFFTKnownTone(t *testing.T) {
	n := 128
	re := make([]float64, n)
	im := make([]float64, n)
	for i := range re {
		re[i] = math.Cos(2 * math.Pi * 5 * float64(i) / float64(n))
	}
	FFT(re, im)
	if math.Abs(re[5]-float64(n)/2) > 1e-9 || math.Abs(re[n-5]-float64(n)/2) > 1e-9 || math.Abs(re[6]) > 1e-9 {
		t.Fatalf("bad spectrum: %v %v %v", re[5], re[n-5], re[6])
	}
}

func TestDecimatorPassband(t *testing.T) {
	in := sine(1000, 48000)
	out := NewDecimator3().Process(in)
	if len(out) != 16000 {
		t.Fatalf("len=%d", len(out))
	}
	db := 20 * math.Log10(rms(out[500:])/rms(in[1500:]))
	t.Logf("1 kHz gain %.3f dB", db)
	if math.Abs(db) > 0.5 {
		t.Fatalf("passband gain %.3f dB", db)
	}
}

func TestDecimatorStopband(t *testing.T) {
	in := sine(10000, 48000)
	out := NewDecimator3().Process(in)
	db := 20 * math.Log10(rms(out[500:])/rms(in[1500:]))
	t.Logf("10 kHz gain %.3f dB", db)
	if db > -40 {
		t.Fatalf("stopband only %.3f dB down", db)
	}
}

func TestDecimatorStreamingEqualsBatch(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	in := make([]float32, 960*10)
	for i := range in {
		in[i] = float32(rng.NormFloat64() * 1000)
	}
	batch := NewDecimator3().Process(in)
	d := NewDecimator3()
	var st []float32
	for i := 0; i < len(in); i += 960 {
		o := d.Process(in[i : i+960])
		if len(o) != 320 {
			t.Fatalf("chunk out len %d", len(o))
		}
		st = append(st, o...)
	}
	if len(st) != len(batch) {
		t.Fatalf("len %d vs %d", len(st), len(batch))
	}
	for i := range st {
		if st[i] != batch[i] {
			t.Fatalf("i=%d %v != %v", i, st[i], batch[i])
		}
	}
	// odd chunk sizes too
	d = NewDecimator3()
	st = st[:0]
	for i := 0; i < len(in); {
		n := 7 + i%13
		if i+n > len(in) {
			n = len(in) - i
		}
		st = append(st, d.Process(in[i:i+n])...)
		i += n
	}
	if len(st) != len(batch) {
		t.Fatalf("odd len %d vs %d", len(st), len(batch))
	}
	for i := range st {
		if st[i] != batch[i] {
			t.Fatalf("odd i=%d", i)
		}
	}
}

func TestMixerRemovesDC(t *testing.T) {
	m := NewMixer(2)
	var out []float32
	for k := 0; k < 50; k++ { // 1 s
		a := make([]float32, 960)
		b := make([]float32, 960)
		for i := range a {
			a[i], b[i] = 17024, 17024
		}
		out = m.Mix([][]float32{a, b})
	}
	var s float64
	for _, v := range out {
		s += math.Abs(float64(v))
	}
	if s/float64(len(out)) >= 1 {
		t.Fatalf("mean |x| = %v", s/float64(len(out)))
	}
}

func TestMixerAverages(t *testing.T) {
	m := NewMixer(2)
	a := sine(1000, 960)
	b := sine(1000, 960)
	for i := range b {
		b[i] *= 3
	}
	var out []float32
	for k := 0; k < 100; k++ {
		out = m.Mix([][]float32{a, b})
	}
	if r := rms(out) / rms(a); math.Abs(r-2) > 0.05 {
		t.Fatalf("ratio %v", r)
	}
}

func TestDeinterleaveCopies(t *testing.T) {
	blk := []int16{1, 10, 100, 2, 20, 200, 3, 30, 300}
	ch := Deinterleave(blk, 3, []int{0, 2})
	blk[0], blk[2] = 99, 99
	if len(ch) != 2 || len(ch[0]) != 3 || ch[0][0] != 1 || ch[0][2] != 3 || ch[1][1] != 200 {
		t.Fatalf("got %v", ch)
	}
}
