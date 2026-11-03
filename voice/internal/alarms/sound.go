package alarms

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"

	"github.com/hajimehoshi/go-mp3"

	"voice/internal/wav"
)

// SoundInfo is a ringtone as the panel offers it.
type SoundInfo struct {
	Name  string `json:"name"`
	Title string `json:"title"`
}

// DefaultSound is an alarm's ringtone unless another is chosen.
const DefaultSound = "alarm"

// Sounds are the alarm ringtones, the default first. The files are the firmware's,
// extracted by setup.
var Sounds = []SoundInfo{
	{DefaultSound, "Классический"},
	{"alarm_from_alice", "Мягкий"},
	{"birds", "Птицы"},
	{"kids", "Детский"},
}

func knownSound(name string) bool {
	if name == TimerSound {
		return true
	}
	for _, s := range Sounds {
		if s.Name == name {
			return true
		}
	}
	return false
}

// LoadSound reads a ringtone from dir. It always returns something to play: a
// ring must not be silent because a file is missing or broken.
func LoadSound(dir, name string) ([]int16, int) {
	if name == "" {
		name = DefaultSound
	}
	pcm, rate, err := readSound(dir, name)
	if err != nil {
		log.Printf("alarms: sound %q: %v; using the built-in signal", name, err)
		return Fallback()
	}
	return pcm, rate
}

func readSound(dir, name string) ([]int16, int, error) {
	if !knownSound(name) {
		return nil, 0, errors.New("no such ringtone")
	}
	base := filepath.Join(dir, name)
	b, err := os.ReadFile(base + ".wav")
	if err == nil {
		pcm, rate, err := wav.Decode(b)
		if err == nil && len(pcm) == 0 {
			err = errors.New("empty")
		}
		return pcm, rate, err
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, 0, err
	}
	if b, err = os.ReadFile(base + ".mp3"); err != nil {
		return nil, 0, err
	}
	return decodeMP3(b)
}

// decodeMP3 gives mono: the library always decodes to s16 stereo.
func decodeMP3(b []byte) ([]int16, int, error) {
	d, err := mp3.NewDecoder(bytes.NewReader(b))
	if err != nil {
		return nil, 0, fmt.Errorf("mp3: %w", err)
	}
	raw, err := io.ReadAll(d)
	if err != nil {
		return nil, 0, fmt.Errorf("mp3: %w", err)
	}
	pcm := make([]int16, len(raw)/4)
	if len(pcm) == 0 {
		return nil, 0, errors.New("mp3: empty")
	}
	for i := range pcm {
		l := int16(uint16(raw[4*i]) | uint16(raw[4*i+1])<<8)
		r := int16(uint16(raw[4*i+2]) | uint16(raw[4*i+3])<<8)
		pcm[i] = int16((int(l) + int(r)) / 2)
	}
	return pcm, d.SampleRate(), nil
}

const (
	fallbackRate = 16000
	fallbackPeak = 0.8 * math.MaxInt16
)

// Fallback is a built-in ring: two high tones and a pause, loud enough to wake.
func Fallback() ([]int16, int) {
	pcm := make([]int16, 0, fallbackRate)
	tone := func(hz float64, n int) {
		for i := range n {
			// Short ramps at the ends keep the tone from clicking.
			env := min(1, float64(i)/80, float64(n-i)/80)
			pcm = append(pcm, int16(fallbackPeak*env*math.Sin(2*math.Pi*hz*float64(i)/fallbackRate)))
		}
	}
	tone(880, fallbackRate/5)
	tone(1175, fallbackRate/5)
	pcm = append(pcm, make([]int16, fallbackRate*3/5)...)
	return pcm, fallbackRate
}
