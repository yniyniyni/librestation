package alarms

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alarms.json")
	when := time.Date(2026, 11, 6, 7, 0, 0, 0, time.UTC)
	s := State{
		Alarms: []Alarm{{ID: "a", Hour: 7, Days: []int{1, 2}, Title: "работа", Sound: "birds", Music: "The Weeknd",
			SnoozeUntil: when, Armed: when, Fired: when}},
		Timers: []Timer{{ID: "t", Title: "чай", Duration: time.Minute, EndsAt: when, Left: time.Second, Paused: true}},
	}
	if err := Save(path, s); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, s) {
		t.Errorf("got %+v, want %+v", got, s)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temporary file left behind: %v", err)
	}
}

func TestLoadMissing(t *testing.T) {
	got, err := Load(filepath.Join(t.TempDir(), "alarms.json"))
	if err != nil || len(got.Alarms)+len(got.Timers) != 0 {
		t.Errorf("got %+v, %v", got, err)
	}
}

// A broken file must not stop the alarms set after it from working.
func TestLoadCorrupt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alarms.json")
	if err := os.WriteFile(path, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err == nil {
		t.Error("no error for a broken file")
	}
	if len(got.Alarms)+len(got.Timers) != 0 {
		t.Errorf("got %+v", got)
	}
	if b, err := os.ReadFile(path + ".bad"); err != nil || string(b) != "{broken" {
		t.Errorf(".bad: %q, %v", b, err)
	}
	if err := Save(path, State{Alarms: []Alarm{{ID: "a"}}}); err != nil {
		t.Fatal(err)
	}
	if got, err := Load(path); err != nil || len(got.Alarms) != 1 {
		t.Errorf("after save: %+v, %v", got, err)
	}
}
