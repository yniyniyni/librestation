package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// retrainTest is a daemon with word x whose retrains are only recorded.
func retrainTest(t *testing.T) (*Daemon, func() []string) {
	t.Helper()
	d, _ := newTest(t, &fakeAct{}, "pause", false)
	var mu sync.Mutex
	var calls []string
	d.retrain = func(id string) error { mu.Lock(); calls = append(calls, id); mu.Unlock(); return nil }
	os.MkdirAll(d.wordDir("x"), 0o755)
	return d, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), calls...) }
}

func addFalses(t *testing.T, d *Daemon, n int, at time.Time) {
	t.Helper()
	for i := range n {
		p := filepath.Join(d.wordDir("x"), fmt.Sprintf("false-%d-%d.wav", at.UnixNano(), i))
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, at, at)
	}
}

func TestNewFalsesCountSinceTraining(t *testing.T) {
	d, _ := retrainTest(t)
	t0 := time.Now().Add(-time.Hour)
	addFalses(t, d, 4, t0)
	if n := d.newFalses("x"); n != 4 {
		t.Fatalf("before any training: %d", n)
	}
	d.markTrained("x", t0.Add(time.Minute))
	addFalses(t, d, 3, t0.Add(2*time.Minute))
	if n := d.newFalses("x"); n != 3 {
		t.Fatalf("after training: %d new, want 3", n)
	}
}

func TestAutoRetrainAtTen(t *testing.T) {
	d, calls := retrainTest(t)
	d.markTrained("x", time.Now().Add(-time.Hour))
	addFalses(t, d, autoRetrainFalses-1, time.Now())
	d.autoRetrain("x")
	if c := calls(); len(c) != 0 {
		t.Fatalf("retrained at %d: %v", autoRetrainFalses-1, c)
	}
	addFalses(t, d, 1, time.Now())
	d.autoRetrain("x")
	if c := calls(); len(c) != 1 || c[0] != "x" {
		t.Fatalf("calls %v", c)
	}
}

func TestAutoRetrainWaitsForBusy(t *testing.T) {
	d, calls := retrainTest(t)
	addFalses(t, d, autoRetrainFalses, time.Now())
	d.mu.Lock()
	d.train.Active = true
	d.mu.Unlock()
	d.autoRetrain("x")
	if c := calls(); len(c) != 0 {
		t.Fatalf("retrained while training: %v", c)
	}
}

// A hit marked false writes its clip and that write is what starts the
// retrain once the tenth clip is in.
func TestFalseVerdictTriggersRetrain(t *testing.T) {
	d, calls := retrainTest(t)
	d.ring.write(make([]int16, 16000))
	addFalses(t, d, autoRetrainFalses-1, time.Now().Add(-time.Second))
	hit(d)
	if err := d.setVerdict(d.hits[0], "false"); err != nil {
		t.Fatal(err)
	}
	d.waitIO()
	for range 200 {
		if len(calls()) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if c := calls(); len(c) != 1 {
		t.Fatalf("calls %v", c)
	}
}
