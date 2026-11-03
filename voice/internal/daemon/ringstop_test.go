package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

type fakeRing struct {
	on    bool
	stops int
}

func (r *fakeRing) Ringing() bool { return r.on }
func (r *fakeRing) Stop()         { r.stops++; r.on = false }

func TestStopWordsStopTheRing(t *testing.T) {
	for _, cmd := range []string{"стоп", "хватит", "пауза"} {
		d, f, a, _, _ := cmdTest(t)
		r := &fakeRing{on: true}
		d.opt.Ring = r
		say(d, cmd)
		if r.stops != 1 {
			t.Errorf("%s: %d stops", cmd, r.stops)
		}
		idle(t, f, a) // and the music is left for the ring to give back
		if h := lastHit(t, d); h.Action != "ring_stop" || !h.Fired {
			t.Errorf("%s: hit %+v", cmd, h)
		}
	}
}

// The switches say what quick commands do to the music; none may leave an alarm
// that cannot be stopped.
func TestStopWordsWorkWhateverTheSwitches(t *testing.T) {
	for name, set := range map[string]func(d *Daemon){
		"quick off": func(d *Daemon) { d.settings.Quick.Off = true },
		"pause off": func(d *Daemon) { d.settings.Quick.Disabled = map[string]bool{"pause": true} },
		"dry run":   func(d *Daemon) { d.settings.DryRun = true },
	} {
		d, _, _, _, _ := cmdTest(t)
		set(d)
		r := &fakeRing{on: true}
		d.opt.Ring = r
		say(d, "стоп")
		if r.stops != 1 {
			t.Errorf("%s: %d stops", name, r.stops)
		}
	}
}

func TestOtherCommandsDuringRingDoNothing(t *testing.T) {
	for _, cmd := range []string{"дальше", "назад", "громче", "тише", "продолжи"} {
		d, f, a, _, _ := cmdTest(t)
		r := &fakeRing{on: true}
		d.opt.Ring = r
		say(d, cmd)
		idle(t, f, a)
		if r.stops != 0 {
			t.Errorf("%s stopped the ring", cmd)
		}
	}
}

func TestStopWordWithoutRingIsAsBefore(t *testing.T) {
	d, f, _, _, done := cmdTest(t)
	r := &fakeRing{}
	d.opt.Ring = r
	say(d, "дальше")
	<-done
	if r.stops != 0 || len(f.got()) != 1 {
		t.Errorf("stops %d, actuator %v", r.stops, f.got())
	}
}

func TestOwnWordStopsTheRing(t *testing.T) {
	d, f, a, _, _ := cmdTest(t) // word "x" pauses
	r := &fakeRing{on: true}
	d.opt.Ring = r
	d.mu.Lock()
	d.onHit("x", 0.9)
	d.mu.Unlock()
	if r.stops != 1 {
		t.Errorf("%d stops", r.stops)
	}
	idle(t, f, a)
	if h := lastHit(t, d); h.Action != "ring_stop" {
		t.Errorf("hit %+v", h)
	}
}

func TestTriggerWordStopsTheRingAndListens(t *testing.T) {
	d, _, a, _, _ := cmdTest(t)
	d.settings.Words["x"].Action = "assistant"
	r := &fakeRing{on: true}
	d.opt.Ring = r
	d.mu.Lock()
	d.onHit("x", 0.9)
	d.mu.Unlock()
	if r.stops != 1 || a.wakes != 1 {
		t.Errorf("stops %d, wakes %d", r.stops, a.wakes)
	}
}

func TestStockWordStopsTheRingAndListens(t *testing.T) {
	d, _, a, _, _ := cmdTest(t)
	r := &fakeRing{on: true}
	d.opt.Ring = r
	d.mu.Lock()
	d.onStockWake("alisa", "Алиса", 0.9)
	d.mu.Unlock()
	if r.stops != 1 || a.wakes != 1 {
		t.Errorf("stops %d, wakes %d", r.stops, a.wakes)
	}
}

// Said twice, "стоп" must not pause the music the first one just brought back.
func TestSecondStopWordLeavesTheMusic(t *testing.T) {
	d, f, a, _, _ := cmdTest(t)
	r := &fakeRing{on: true}
	d.opt.Ring = r
	say(d, "стоп")
	say(d, "стоп")
	idle(t, f, a)
}

func TestBrokenSettingsDoNotStopTheDaemon(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{DataDir: dir}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path + ".bad"); err != nil || string(b) != "{broken" {
		t.Errorf(".bad: %q, %v", b, err)
	}
}
