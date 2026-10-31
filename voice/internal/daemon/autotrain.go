package daemon

import (
	"log"
	"os"
	"path/filepath"
	"time"
)

// A word is retrained by itself once autoRetrainFalses false-alarm clips have
// been saved for it since its last training, from any source: the user, a
// take-back or the audio model. The marker file's mtime is when the last
// successful training started; clips newer than it are new.

const (
	autoRetrainFalses = 10
	trainedMarker     = "falses-trained"
)

// newFalses counts the word's false-alarm clips saved after its last training.
func (d *Daemon) newFalses(id string) int {
	dir := d.wordDir(id)
	var since time.Time
	if st, err := os.Stat(filepath.Join(dir, trainedMarker)); err == nil {
		since = st.ModTime()
	} else if st, err := os.Stat(filepath.Join(dir, "model.json")); err == nil {
		since = st.ModTime() // trained before markers existed
	}
	falses, _ := filepath.Glob(filepath.Join(dir, "false-*.wav"))
	n := 0
	for _, p := range falses {
		if st, err := os.Stat(p); err == nil && st.ModTime().After(since) {
			n++
		}
	}
	return n
}

// markTrained notes that a training which started at start succeeded.
func (d *Daemon) markTrained(id string, start time.Time) {
	p := filepath.Join(d.wordDir(id), trainedMarker)
	err := os.WriteFile(p, nil, 0o644)
	if err == nil {
		err = os.Chtimes(p, start, start)
	}
	if err != nil {
		log.Printf("train %s: marker: %v", id, err)
	}
}

// autoRetrain retrains id when enough new false clips have piled up and
// nothing else is training or recording.
func (d *Daemon) autoRetrain(id string) {
	d.mu.Lock()
	busy := d.train.Active || d.enroll != nil || d.settings.Words[id] == nil
	d.mu.Unlock()
	if busy {
		return
	}
	n := d.newFalses(id)
	if n < autoRetrainFalses {
		return
	}
	log.Printf("auto retrain %s: %d new false alarms", id, n)
	retrain := d.retrain
	if retrain == nil {
		retrain = func(id string) error { return d.startTraining(id, nil) }
	}
	if err := retrain(id); err != nil {
		log.Printf("auto retrain %s: %v", id, err)
	}
}

// autoRetrainAny gives every word its chance, e.g. after a training ends.
func (d *Daemon) autoRetrainAny() {
	d.mu.Lock()
	ids := make([]string, 0, len(d.words))
	for _, w := range d.words {
		ids = append(ids, w.ID)
	}
	d.mu.Unlock()
	for _, id := range ids {
		d.autoRetrain(id)
	}
}
