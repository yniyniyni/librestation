// Package volume keeps the one volume number the user sees and controls:
// the Spotify slider, the rotary knob and the louder/quieter voice actions
// all move the same value.
//
// Master volume decision (Task 9 investigation on the station, 2026-10-29):
//
//   - Hardware volume IS settable without io.sdk: AudioSystem.setParameters
//     ("hw_master_volume=N") makes the Amlogic HAL write the TAS5825 control
//     ("audio_hardvol: Set mixer ctrl: ret=0; name=\"Tweeters DSP Playback
//     Volume\"; value=109" for N=14). AudioManager stream volume does not.
//   - Soloist DOES attenuate in software: its volume is the PulseAudio
//     sink-input volume of the "spotify" stream (Soloist 15 -> "15% / -49.44 dB",
//     10 -> "10% / -60.00 dB"), applied before the pipe-sink FIFO.
//
// Per the rule in the plan, Soloist's volume is therefore the master. The
// hardware level is pinned once at daemon start (cmd/voiced, -hw-volume,
// default 14 = the level the station already had) and never touched again, so
// Soloist 15 sounds exactly as loud as before the switch and Soloist 100 equals
// the old ceiling of hardware 100 at Soloist 15 (both about -49 dB). Sync only
// talks to Soloist: knob and voice steps send set_volume; a volume that Soloist
// reports and we did not send (the Spotify slider) is adopted and saved, never
// sent back, so there is no feedback loop.
package volume

import (
	"context"
	"log"
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
	// Step is the change of one knob detent.
	Step = 5
	// echoWindow is how long a value we sent is expected to come back from
	// Soloist; reports that disagree with it meanwhile are stale.
	echoWindow  = 2 * time.Second
	maxInflight = 16
)

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

	now   func() time.Time
	retry time.Duration

	mu       sync.Mutex
	cur      int
	applied  bool
	inflight []sent
}

// New loads the saved volume from path (Default if missing or invalid).
// Nothing is sent until Apply or Run.
func New(set Setter, path string) *Sync {
	s := &Sync{set: set, path: path, now: time.Now, retry: time.Second, cur: Default}
	if b, err := os.ReadFile(path); err == nil {
		if v, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && v >= 0 && v <= 100 {
			s.cur = v
		}
	}
	return s
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
	switch code {
	case input.KeyPageUp:
		s.Step(Step)
	case input.KeyPageDown:
		s.Step(-Step)
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
	return nil
}

// save writes the volume atomically; a failure only costs persistence.
func (s *Sync) save() {
	tmp := s.path + ".tmp"
	err := os.MkdirAll(filepath.Dir(s.path), 0o755)
	if err == nil {
		err = os.WriteFile(tmp, []byte(strconv.Itoa(s.cur)+"\n"), 0o644)
	}
	if err == nil {
		err = os.Rename(tmp, s.path)
	}
	if err != nil {
		log.Printf("volume: save: %v", err)
	}
}
