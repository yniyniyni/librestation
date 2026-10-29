// Package volume keeps the one volume number the user sees and controls:
// the Spotify slider, the rotary knob and the louder/quieter voice actions
// all move the same value.
//
// How the number becomes loudness (revised 2026-10-29 after live use):
//
//   - Soloist attenuates in software: its volume is the PulseAudio sink-input
//     volume of the "spotify" stream, on PulseAudio's cubic scale (Soloist 15 ->
//     "15% / -49.44 dB"). Soloist also adopts any outside change of that
//     sink-input volume as its own, so it cannot simply be reset to 100%.
//   - With the amplifier pinned, that curve made 15% inaudible and only ~100%
//     usable. So the software attenuation is undone after PulseAudio instead:
//     the sink runs in float, and aaudio_pipe multiplies by Compensation(v).
//   - The amplifier carries the loudness: AudioSystem.setParameters
//     ("hw_master_volume=N") makes the Amlogic HAL write the TAS5825 controls;
//     N follows the volume one to one.
//
// Sync only talks to Soloist: knob and voice steps send set_volume; a volume
// that Soloist reports and we did not send (the Spotify slider) is adopted and
// saved, never sent back, so there is no feedback loop. Every volume that
// becomes current is handed to Out, which drives the amplifier and the gain.
package volume

import (
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"voice/internal/input"
	"voice/internal/soloist"
)

const (
	// Default is the volume applied at start when nothing is saved.
	Default = 15
	// DefaultKnobStep is the change of one knob detent unless set otherwise.
	DefaultKnobStep = 2
	// MaxKnobStep bounds SetKnobStep.
	MaxKnobStep = 10
	// echoWindow is how long a value we sent is expected to come back from
	// Soloist; reports that disagree with it meanwhile are stale.
	echoWindow  = 2 * time.Second
	maxInflight = 16
)

// Compensation is the gain that undoes Soloist's software attenuation at
// volume v: PulseAudio's cubic scale gives (v/100)^3. Volume 0 stays silent.
func Compensation(v int) float64 {
	if v <= 0 {
		return 0
	}
	return math.Pow(100/float64(min(v, 100)), 3)
}

// Setter is the part of the Soloist client Sync needs.
type Setter interface {
	SetVolume(v int) error
}

type sent struct {
	v  int
	at time.Time
}

// Sync is the single volume. Its methods are safe for concurrent use;
// OnEvent never blocks on the network.
type Sync struct {
	set  Setter
	path string
	// Out, when set before Apply/Run, gets every volume that becomes current
	// (ours or adopted from Spotify). It is called with the lock held and must
	// not block.
	Out func(v int)

	now   func() time.Time
	retry time.Duration

	mu       sync.Mutex
	cur      int
	knobStep int
	applied  bool
	inflight []sent
}

// New loads the saved volume from path (Default if missing or invalid).
// Nothing is sent until Apply or Run.
func New(set Setter, path string) *Sync {
	s := &Sync{set: set, path: path, now: time.Now, retry: time.Second, cur: Default, knobStep: DefaultKnobStep}
	if v, ok := readInt(path); ok && v >= 0 && v <= 100 {
		s.cur = v
	}
	if v, ok := readInt(path + "-knob-step"); ok && v >= 1 && v <= MaxKnobStep {
		s.knobStep = v
	}
	return s
}

func readInt(path string) (int, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return v, err == nil
}

// KnobStep is the change of one knob detent.
func (s *Sync) KnobStep() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.knobStep
}

// SetKnobStep sets and saves the knob step, 1..MaxKnobStep.
func (s *Sync) SetKnobStep(n int) error {
	if n < 1 || n > MaxKnobStep {
		return fmt.Errorf("knob step must be 1..%d", MaxKnobStep)
	}
	s.mu.Lock()
	s.knobStep = n
	s.mu.Unlock()
	return writeAtomic(s.path+"-knob-step", n)
}

// Volume is the current volume 0..100.
func (s *Sync) Volume() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// Apply sends the start volume to Soloist once. Later calls do nothing.
func (s *Sync) Apply() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.applied {
		return nil
	}
	return s.sendLocked(s.cur)
}

// Run applies the start volume, retrying while Soloist is unreachable, then
// returns. It returns early when ctx is cancelled.
func (s *Sync) Run(ctx context.Context) {
	for s.Apply() != nil {
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.retry):
		}
	}
}

// Step changes the volume by delta, clamped to 0..100 (daemon.Actuator).
func (s *Sync) Step(delta int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := min(max(s.cur+delta, 0), 100)
	if v == s.cur && s.applied {
		return nil
	}
	return s.sendLocked(v)
}

// Key handles a knob key press: PAGEUP louder, PAGEDOWN quieter.
func (s *Sync) Key(code uint16) {
	n := s.KnobStep()
	switch code {
	case input.KeyPageUp:
		s.Step(n)
	case input.KeyPageDown:
		s.Step(-n)
	}
}

// OnEvent takes Soloist events. A reported volume that echoes what we sent is
// ignored; any other one is a change made in Spotify and becomes current.
func (s *Sync) OnEvent(e soloist.Event) {
	if e.Volume == nil {
		return
	}
	v := *e.Volume
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.applied {
		return // Soloist's own start value; ours replaces it
	}
	now := s.now()
	live := s.inflight[:0]
	for _, x := range s.inflight {
		if now.Sub(x.at) < echoWindow {
			live = append(live, x)
		}
	}
	s.inflight = live
	for i, x := range s.inflight {
		if x.v == v {
			s.inflight = s.inflight[i+1:]
			return // our own echo
		}
	}
	if len(s.inflight) > 0 || v == s.cur {
		return // stale report from before our latest set
	}
	s.cur = v
	s.save()
	s.outLocked()
}

func (s *Sync) outLocked() {
	if s.Out != nil {
		s.Out(s.cur)
	}
}

func (s *Sync) sendLocked(v int) error {
	if err := s.set.SetVolume(v); err != nil {
		return err
	}
	s.applied = true
	if len(s.inflight) == maxInflight {
		s.inflight = s.inflight[1:]
	}
	s.inflight = append(s.inflight, sent{v, s.now()})
	if v != s.cur {
		s.cur = v
		s.save()
	}
	s.outLocked()
	return nil
}

// save writes the volume; a failure only costs persistence.
func (s *Sync) save() {
	if err := writeAtomic(s.path, s.cur); err != nil {
		log.Printf("volume: save: %v", err)
	}
}

func writeAtomic(path string, v int) error {
	tmp := path + ".tmp"
	err := os.MkdirAll(filepath.Dir(path), 0o755)
	if err == nil {
		err = os.WriteFile(tmp, []byte(strconv.Itoa(v)+"\n"), 0o644)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	return err
}
