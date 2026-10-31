package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"voice/internal/kws"
	"voice/internal/wav"
)

// Hits are kept in <data>/hits as <id>.json + <id>.wav so the review queue
// survives restarts. Writes run off the audio goroutine.

// maxHits is how many recent hits are kept, in memory and on disk.
const maxHits = 50

// hitSecs is the audio kept per hit, ending at the detection.
const hitSecs = 3

const (
	// undoWindow: a fired action reverted this soon (by voice, in the Spotify
	// app or with the knob) was most likely a false alarm.
	undoWindow = 10 * time.Second
	// undoGuard: changes this soon after an action are its own echo or a
	// stale report from before it.
	undoGuard = 700 * time.Millisecond
)

var oppositeAction = map[string]string{
	"pause": "play", "play": "pause",
	"next": "prev", "prev": "next",
}

func (d *Daemon) hitsDir() string { return filepath.Join(d.opt.DataDir, "hits") }

func (d *Daemon) hitPath(id int, ext string) string {
	return filepath.Join(d.hitsDir(), strconv.Itoa(id)+ext)
}

// loadHits reads the saved hits (newest maxHits); audio is read on demand.
func (d *Daemon) loadHits() error {
	if err := os.MkdirAll(d.hitsDir(), 0o755); err != nil {
		return err
	}
	files, _ := filepath.Glob(filepath.Join(d.hitsDir(), "*.json"))
	var hits []*Hit
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		h := &Hit{}
		if err := json.Unmarshal(b, h); err != nil || h.ID <= 0 {
			log.Printf("hits: skip %s: %v", p, err)
			continue
		}
		h.volBefore = -1
		hits = append(hits, h)
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].ID < hits[j].ID })
	for len(hits) > maxHits {
		d.removeHitFiles(hits[0].ID)
		hits = hits[1:]
	}
	d.hits = hits
	if len(hits) > 0 {
		d.hitSeq = hits[len(hits)-1].ID
	}
	return nil
}

// addHitLocked appends a new hit and drops the oldest; caller holds d.mu.
func (d *Daemon) addHitLocked(h *Hit) {
	d.hitSeq++
	h.ID = d.hitSeq
	d.hits = append(d.hits, h)
	for len(d.hits) > maxHits {
		old := d.hits[0]
		old.pruned = true
		d.hits = d.hits[1:]
		d.persist(old)
	}
	d.persist(h)
}

// persist saves h in the background.
func (d *Daemon) persist(h *Hit) {
	d.io.Add(1)
	go func() {
		defer d.io.Done()
		d.saveHit(h)
	}()
}

// waitIO waits for background hit writes (tests, shutdown).
func (d *Daemon) waitIO() { d.io.Wait() }

// saveHit writes h's current state; caller must not hold d.mu. Writes are
// serialised and each one takes a fresh snapshot, so a later state is never
// overwritten by an earlier one.
func (d *Daemon) saveHit(h *Hit) {
	d.ioMu.Lock()
	defer d.ioMu.Unlock()
	d.mu.Lock()
	b, _ := json.MarshalIndent(h, "", "  ")
	id, word, falseFile, audio, pruned := h.ID, h.Word, h.FalseFile, h.audio, h.pruned
	d.mu.Unlock()
	if pruned {
		d.removeHitFiles(id)
		return
	}
	wavPath := d.hitPath(id, ".wav")
	if _, err := os.Stat(wavPath); errors.Is(err, os.ErrNotExist) && audio != nil {
		if err := wav.WriteFile(wavPath, audio, kws.SampleRate); err != nil {
			log.Printf("hits: %v", err)
		}
	}
	if falseFile != "" {
		p := filepath.Join(d.wordDir(word), falseFile)
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			if audio == nil {
				audio, _ = wav.ReadFile(wavPath, kws.SampleRate)
			}
			os.MkdirAll(filepath.Dir(p), 0o755)
			if err := wav.WriteFile(p, audio, kws.SampleRate); err != nil {
				log.Printf("hits: false clip: %v", err)
			} else {
				go d.autoRetrain(word)
			}
		}
	}
	tmp := d.hitPath(id, ".json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Printf("hits: %v", err)
		return
	}
	os.Rename(tmp, d.hitPath(id, ".json"))
}

func (d *Daemon) removeHitFiles(id int) {
	os.Remove(d.hitPath(id, ".json"))
	os.Remove(d.hitPath(id, ".wav"))
}

// hitAudio returns the hit's clip, from memory or disk.
func (d *Daemon) hitAudio(h *Hit) ([]int16, error) {
	d.mu.Lock()
	a := h.audio
	d.mu.Unlock()
	if a != nil {
		return a, nil
	}
	return wav.ReadFile(d.hitPath(h.ID, ".wav"), kws.SampleRate)
}

// markFalseLocked makes h a false alarm: its clip becomes a hard negative of
// the word at the next training. A Yandex command has nothing to train: it
// is only marked. Caller holds d.mu and persists h.
func (d *Daemon) markFalseLocked(h *Hit, auto bool) {
	h.Verdict, h.Auto = "false", auto
	if h.FalseFile == "" && h.Source != "yandex" {
		h.FalseFile = fmt.Sprintf("false-%d.wav", h.At.Unix())
	}
}

// setVerdict is the user's call on a hit: "false" or "ok". Saving "ok"
// takes back a false-alarm clip that was saved for it.
func (d *Daemon) setVerdict(h *Hit, verdict string) error {
	switch verdict {
	case "false":
		d.mu.Lock()
		d.markFalseLocked(h, false)
		d.mu.Unlock()
	case "ok":
		d.ioMu.Lock()
		d.mu.Lock()
		ff := h.FalseFile
		h.Verdict, h.Auto, h.FalseFile = "ok", false, ""
		d.mu.Unlock()
		if ff != "" {
			os.Remove(filepath.Join(d.wordDir(h.Word), ff))
		}
		d.ioMu.Unlock()
	default:
		return fmt.Errorf("unknown verdict %q", verdict)
	}
	d.saveHit(h)
	return nil
}

// revertedLocked notes that action just took effect (said, or seen in
// Soloist's playback state) and flags the latest fired hit if it is the
// opposite: the user took the command back. Caller holds d.mu.
func (d *Daemon) revertedLocked(action string) {
	want := oppositeAction[action]
	if want == "" {
		return
	}
	now := d.now()
	for i := len(d.hits) - 1; i >= 0; i-- {
		h := d.hits[i]
		if !h.Fired {
			continue
		}
		age := now.Sub(h.At)
		if h.Action == want && h.Undone == 0 && age >= undoGuard && age <= undoWindow {
			d.flagUndoLocked(h, age)
		}
		return // only the latest command can be taken back
	}
}

// ObservePlayback takes Soloist's playback status ("playing", "paused").
// Music counts as playing until the status leaves "playing".
func (d *Daemon) ObservePlayback(status string) {
	var action string
	switch status {
	case "playing":
		action = "play"
	case "paused":
		action = "pause"
	default:
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	prev := d.playback
	d.playback = status
	if status == "playing" || prev == "playing" {
		d.lastPlaying = d.now()
	}
	if prev != "" && prev != status {
		d.revertedLocked(action)
	}
}

// ObserveVolume takes every volume that becomes current, whoever set it.
// A louder/quieter hit counts as taken back when the volume moved its way
// and then returned to where it was within undoWindow.
func (d *Daemon) ObserveVolume(v int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.volume = v
	now := d.now()
	for i := len(d.hits) - 1; i >= 0; i-- {
		h := d.hits[i]
		age := now.Sub(h.At)
		if age > undoWindow {
			break
		}
		if !h.Fired || h.Undone > 0 || h.volBefore < 0 {
			continue
		}
		switch {
		case h.Action == "louder" && v > h.volBefore, h.Action == "quieter" && v < h.volBefore:
			h.volMoved = true
		case h.volMoved && age >= undoGuard &&
			(h.Action == "louder" && v <= h.volBefore || h.Action == "quieter" && v >= h.volBefore):
			d.flagUndoLocked(h, age)
		}
	}
}

func (d *Daemon) flagUndoLocked(h *Hit, age time.Duration) {
	h.Undone = max(0.1, float64(age.Round(100*time.Millisecond))/float64(time.Second))
	log.Printf("hit %d %s: %s taken back after %.1f s", h.ID, h.Word, h.Action, h.Undone)
	if d.settings.AutoFalseUndo && h.Verdict == "" {
		d.markFalseLocked(h, true)
	}
	d.persist(h)
}
