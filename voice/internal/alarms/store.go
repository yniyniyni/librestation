package alarms

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// State is what lives on disk.
type State struct {
	Alarms []Alarm `json:"alarms"`
	Timers []Timer `json:"timers"`
	// Ring is the ring on now: voiced restarted in the middle of it goes on.
	Ring *RingRecord `json:"ring,omitempty"`
}

// RingRecord is a ring as it is kept across a restart.
type RingRecord struct {
	Ring     Ring      `json:"ring"`
	AlarmID  string    `json:"alarm_id,omitempty"`
	Since    time.Time `json:"since"`
	Deadline time.Time `json:"deadline"`
}

// Load reads the state. A missing file is an empty state. A broken one is kept
// beside it as .bad and also gives an empty state, with the error: alarms set
// from now on must still work.
func Load(path string) (State, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, nil
	}
	if err != nil {
		return State{}, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		if e := os.Rename(path, path+".bad"); e != nil {
			return State{}, fmt.Errorf("%s: %w; %v", path, err, e)
		}
		return State{}, fmt.Errorf("%s: %w; kept as .bad", path, err)
	}
	return s, nil
}

// Save writes the state atomically and durably: a power cut right after must
// leave either the old file or the new one, not a broken one.
func Save(path string, s State) error {
	b, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		dir.Sync()
		dir.Close()
	}
	return nil
}
