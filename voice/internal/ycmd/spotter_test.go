package ycmd

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"voice/internal/capture"
	"voice/internal/wav"
)

type hit struct {
	cmd  string
	conf float64
}

// pushAll feeds pcm in chunks of n samples and returns the fires.
func pushAll(s *Spotter, pcm []float32, n int) []hit {
	var hits []hit
	fire := func(cmd string, conf float64) { hits = append(hits, hit{cmd, conf}) }
	for len(pcm) > 0 {
		k := min(n, len(pcm))
		s.Push(pcm[:k], fire)
		pcm = pcm[k:]
	}
	return hits
}

// words is 4 s of alternating low and high tones with quiet between, over
// a little deterministic noise: on the synthetic model the low tone pushes
// posterior 1 (включи) up, the high tone posterior 2 (свет).
func words() []float32 {
	x := make([]float32, 4*16000)
	seed := uint32(1)
	for i := range x {
		seed = seed*1664525 + 1013904223
		x[i] = float32(int32(seed)>>20) / 64 // about ±32
		seg := i / 4000                      // quarter-second segments
		if hz := map[int]float64{1: 300, 5: 3000, 9: 300, 13: 3000}[seg]; hz > 0 {
			x[i] += float32(4000 * math.Sin(2*math.Pi*hz*float64(i)/16000))
		}
	}
	return x
}

func TestPushChunking(t *testing.T) {
	m := synthModel(t)
	pcm := words()
	var first []hit
	for _, n := range []int{320, 7, 4096} {
		got := pushAll(NewSpotter(m), pcm, n)
		if n == 320 {
			first = got
			var names []string
			for _, h := range got {
				names = append(names, h.cmd)
			}
			if strings.Join(names, " ") != "включи свет включи свет" {
				t.Fatalf("chunks of 320: fires %+v, want one per tone", got)
			}
			continue
		}
		if fmt.Sprint(got) != fmt.Sprint(first) {
			t.Errorf("chunks of %d: %+v, want %+v", n, got, first)
		}
	}
	// Reset forgets everything: the same signal fires the same way again.
	s := NewSpotter(m)
	pushAll(s, pcm[:30000], 999)
	s.Reset()
	if got := pushAll(s, pcm, 320); fmt.Sprint(got) != fmt.Sprint(first) {
		t.Errorf("after Reset: %+v, want %+v", got, first)
	}
}

func TestPushNoAlloc(t *testing.T) {
	m := synthModel(t)
	s := NewSpotter(m)
	pcm := words()
	fires := 0
	fire := func(string, float64) { fires++ }
	s.Push(pcm[:16000], fire) // warm up
	pos := 16000
	allocs := testing.AllocsPerRun(100, func() {
		s.Push(pcm[pos:pos+320], fire)
		pos = 16000 + (pos-16000+320)%(len(pcm)-16320)
	})
	if allocs != 0 {
		t.Errorf("Push allocates %v times per 20 ms", allocs)
	}
	if fires == 0 {
		t.Error("no fires while measuring; the decoder path went untested")
	}
}

// rawCh0 reads a 16-channel 48 kHz capture and runs it through Input the
// way voiced feeds the spotter. The reader keeps channel 0 only, so the
// other channels of each block are left silent.
func rawCh0(t *testing.T, path string) []float32 {
	t.Helper()
	ch0, err := wav.ReadFile(path, capture.Rate) // keeps channel 0
	if err != nil {
		t.Fatal(err)
	}
	in := NewInput()
	blk := make([]int16, capture.BlockFrames*capture.Channels)
	var out []float32
	for p := 0; p+capture.BlockFrames <= len(ch0); p += capture.BlockFrames {
		for i, v := range ch0[p : p+capture.BlockFrames] {
			blk[i*capture.Channels] = v
		}
		out = append(out, in.Process(blk)...)
	}
	return out
}

func TestRealClips(t *testing.T) {
	dir, clips := os.Getenv("YCMD_DIR"), os.Getenv("YCMD_CLIPS")
	if dir == "" || clips == "" {
		t.Skip("YCMD_DIR or YCMD_CLIPS not set")
	}
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	single := map[string]bool{}
	for _, c := range m.Commands {
		single[c.Name] = len(c.Labels) == 1
	}
	spoken := strings.Fields("стоп пауза продолжи дальше назад громче тише хватит следующий")
	for _, name := range []string{"near", "far", "music", "talk", "ambient"} {
		pcm := rawCh0(t, filepath.Join(clips, name+".wav"))
		var hits []string
		var singles []string
		samples := 0
		s := NewSpotter(m)
		for len(pcm) > 0 {
			k := min(320, len(pcm))
			s.Push(pcm[:k], func(cmd string, conf float64) {
				hits = append(hits, fmt.Sprintf("%.2fs %s %.3f", float64(samples+k)/16000, cmd, conf))
				if single[cmd] {
					singles = append(singles, cmd)
				}
			})
			samples += k
			pcm = pcm[k:]
		}
		t.Logf("%s: %d fires: %s", name, len(hits), strings.Join(hits, ", "))
		switch name {
		case "talk", "ambient":
			if len(hits) != 0 {
				t.Errorf("%s: %d fires, want none", name, len(hits))
			}
		default:
			// Exactly one fire per spoken command: a double fire would
			// be a double volume step in the daemon.
			if len(hits) != len(spoken) || !slices.Equal(singles, spoken) {
				t.Errorf("%s: %d fires, single commands %q, want exactly %q", name, len(hits), singles, spoken)
			}
		}
	}
}

func BenchmarkFrame(b *testing.B) {
	dir := os.Getenv("YCMD_DIR")
	if dir == "" {
		b.Skip("YCMD_DIR not set")
	}
	m, err := Load(dir)
	if err != nil {
		b.Fatal(err)
	}
	s := NewSpotter(m)
	pcm := sine(1000, 440, 16000)
	s.Push(pcm, nil)
	b.ResetTimer()
	for i := range b.N {
		k := 160 * (i % 99)
		s.Push(pcm[k:k+160], nil) // one 10 ms frame
	}
}

// BenchmarkBlock pushes 20 ms blocks as the daemon does, so each op runs
// two frames through the network together; ns/frame is half of ns/op.
func BenchmarkBlock(b *testing.B) {
	dir := os.Getenv("YCMD_DIR")
	if dir == "" {
		b.Skip("YCMD_DIR not set")
	}
	m, err := Load(dir)
	if err != nil {
		b.Fatal(err)
	}
	s := NewSpotter(m)
	pcm := sine(1000, 440, 16000)
	s.Push(pcm, nil)
	b.ResetTimer()
	for i := range b.N {
		k := 320 * (i % 49)
		s.Push(pcm[k:k+320], nil) // one 20 ms block
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(2*b.N), "ns/frame")
}
