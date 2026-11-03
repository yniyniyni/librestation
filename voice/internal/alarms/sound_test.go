package alarms

import (
	"os"
	"path/filepath"
	"testing"

	"voice/internal/wav"
)

func sameAsFallback(pcm []int16, rate int) bool {
	f, fr := Fallback()
	return rate == fr && len(pcm) == len(f)
}

func TestFallbackIsLoud(t *testing.T) {
	pcm, rate := Fallback()
	if rate <= 0 || len(pcm) == 0 {
		t.Fatalf("rate %d, %d samples", rate, len(pcm))
	}
	peak := 0
	for _, s := range pcm {
		peak = max(peak, int(s), -int(s))
	}
	if peak < 22000 {
		t.Errorf("peak %d", peak)
	}
}

func TestLoadSoundWAV(t *testing.T) {
	dir := t.TempDir()
	if err := wav.WriteFile(filepath.Join(dir, "birds.wav"), []int16{1, 2, 3}, 48000); err != nil {
		t.Fatal(err)
	}
	pcm, rate := LoadSound(dir, "birds")
	if rate != 48000 || len(pcm) != 3 {
		t.Errorf("rate %d, pcm %v", rate, pcm)
	}
}

func TestLoadSoundDefault(t *testing.T) {
	dir := t.TempDir()
	if err := wav.WriteFile(filepath.Join(dir, DefaultSound+".wav"), []int16{1, 2}, 16000); err != nil {
		t.Fatal(err)
	}
	if pcm, _ := LoadSound(dir, ""); len(pcm) != 2 {
		t.Errorf("pcm %v", pcm)
	}
}

func TestLoadSoundFallsBack(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "kids.wav"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "alarm.mp3"), []byte("junk junk junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"birds", "kids", "alarm", "timer"} {
		if pcm, rate := LoadSound(dir, name); !sameAsFallback(pcm, rate) {
			t.Errorf("%s: %d samples at %d", name, len(pcm), rate)
		}
	}
}

// A name comes from the panel; it must not reach outside the directory.
func TestLoadSoundOnlyKnownNames(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "alarm")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := wav.WriteFile(filepath.Join(parent, "x.wav"), []int16{1, 2, 3}, 16000); err != nil {
		t.Fatal(err)
	}
	if pcm, rate := LoadSound(dir, "../x"); !sameAsFallback(pcm, rate) {
		t.Errorf("read outside: %d samples", len(pcm))
	}
}

// The firmware's MP3 is not in the repository: ALARMS_TEST_MP3 names one.
func TestLoadSoundMP3(t *testing.T) {
	src := os.Getenv("ALARMS_TEST_MP3")
	if src == "" {
		t.Skip("ALARMS_TEST_MP3 not set")
	}
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "alarm.mp3"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	pcm, rate := LoadSound(dir, "alarm")
	if sameAsFallback(pcm, rate) || rate != 48000 || len(pcm) < 48000 {
		t.Errorf("%d samples at %d", len(pcm), rate)
	}
}

// A sound that cannot be played must not reach the speaker, which would refuse
// it again and again in silence.
func TestLoadSoundZeroRate(t *testing.T) {
	dir := t.TempDir()
	if err := wav.WriteFile(filepath.Join(dir, "birds.wav"), []int16{1, 2, 3}, 0); err != nil {
		t.Fatal(err)
	}
	if pcm, rate := LoadSound(dir, "birds"); !sameAsFallback(pcm, rate) {
		t.Errorf("%d samples at %d", len(pcm), rate)
	}
}

// The MP3 library panics on some broken files; the ring must go on.
func TestDecoderPanicIsAnError(t *testing.T) {
	_, _, err := guarded(func() ([]int16, int, error) { panic("index out of range") })
	if err == nil {
		t.Error("no error")
	}
}
