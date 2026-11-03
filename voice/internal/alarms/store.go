package alarms

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// State is what lives on disk.
type State struct {
	Alarms []Alarm `json:"alarms"`
	Timers []Timer `json:"timers"`
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

// Save writes the state atomically.
func Save(path string, s State) error {
	b, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
