package ycmd

import "testing"

type firing struct {
	frame int
	cmd   int
	conf  float64
}

// feed pushes one posterior frame per value pair (label 1, label 2) and
// returns the fires; frames are numbered from start.
func feed(d *decoder, start int, frames [][2]float32) []firing {
	var out []firing
	for i, p := range frames {
		if c, conf := d.push([]float32{0, p[0], p[1]}); c >= 0 {
			out = append(out, firing{start + i, c, conf})
		}
	}
	return out
}

// rise is a word of command A: below threshold, five rising frames above
// it peaking at 0.95, then falling. Command B hums along below threshold.
var rise = [][2]float32{{0.1, 0.3}, {0.6, 0.3}, {0.7, 0.3}, {0.8, 0.3}, {0.9, 0.3}, {0.95, 0.3}, {0.4, 0.3}, {0.1, 0.3}}

func silence(n int) [][2]float32 { return make([][2]float32, n) }

func TestDecoderPeakAndRearm(t *testing.T) {
	cmds := []Command{{Name: "a", Labels: []int{1}, Threshold: 0.5}, {Name: "b", Labels: []int{2}, Threshold: 0.5}}
	// No smoothing; confidence is the max of the last 4 posteriors.
	d := newDecoder(cmds, 1, 4, 10, 3)

	got := feed(d, 0, rise[:7])
	// The peak is frame 5; frame 6 is the first that does not rise.
	if len(got) != 1 || got[0] != (firing{6, 0, float64(float32(0.95))}) {
		t.Fatalf("fires %+v, want one of a at frame 6 with 0.95", got)
	}
	// The hit zeroed every command's windows: one silent frame and all
	// confidences are gone, b's included.
	feed(d, 7, silence(1))
	for i, c := range d.conf {
		if c > 1e-9 {
			t.Errorf("after the hit, command %d confidence %v", i, c)
		}
	}

	// Frames 8..16 are still within the 10 quiet frames after frame 6: a
	// second word of a peaking there does not fire, then or later.
	if got := feed(d, 8, append(append([][2]float32{}, rise...), silence(20)...)); len(got) != 0 {
		t.Errorf("repeat within rearm fired: %+v", got)
	}
	// Later the same word fires again.
	got = feed(d, 36, rise)
	if len(got) != 1 || got[0].cmd != 0 || got[0].frame != 42 {
		t.Errorf("repeat after rearm: %+v, want a at frame 42", got)
	}
}

func TestDecoderSmoothingAndPairs(t *testing.T) {
	cmds := []Command{{Name: "a b", Labels: []int{1, 2}, Threshold: 0.4}}
	d := newDecoder(cmds, 2, 6, 5, 3)
	d.push([]float32{0, 0.8, 0})
	d.push([]float32{0, 0.8, 0})
	// Smoothed: label 1 peaks at 0.8; label 2 is 0.1 then 0.2 here, so the
	// geometric mean of the window maxima is sqrt(0.8*0.2).
	d.push([]float32{0, 0, 0.2})
	d.push([]float32{0, 0, 0.2})
	if want := 0.4; d.conf[0] < want-1e-6 || d.conf[0] > want+1e-6 {
		t.Errorf("confidence %v, want %v", d.conf[0], want)
	}
}
