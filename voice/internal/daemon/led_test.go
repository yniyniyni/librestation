package daemon

import (
	"sync"
	"testing"
	"time"

	"voice/internal/led/proto"
)

type ledLog struct {
	mu sync.Mutex
	ev []proto.Event
}

func (l *ledLog) send(e proto.Event) { l.mu.Lock(); l.ev = append(l.ev, e); l.mu.Unlock() }
func (l *ledLog) kind(t string) []proto.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []proto.Event
	for _, e := range l.ev {
		if e.T == t {
			out = append(out, e)
		}
	}
	return out
}

func newLEDTest(t *testing.T, action string, dry bool) (*Daemon, *ledLog) {
	t.Helper()
	l := &ledLog{}
	d, err := New(Options{DataDir: t.TempDir(), Actuator: &fakeAct{}, OnLED: l.send})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.waitIO)
	d.settings.Words["x"] = &WordSettings{Enabled: true, Action: action}
	d.settings.DryRun = dry
	return d, l
}

func TestOnLEDCommandOnFire(t *testing.T) {
	d, l := newLEDTest(t, "pause", false)
	hit(d)
	hit(d) // debounced
	cmds := l.kind("cmd")
	if len(cmds) != 1 || cmds[0].A != "pause" {
		t.Fatalf("cmd events %+v", cmds)
	}

	d, l = newLEDTest(t, "pause", true)
	hit(d)
	if n := len(l.kind("cmd")); n != 0 {
		t.Fatalf("dry-run sent %d cmd events", n)
	}

	d, l = newLEDTest(t, "louder", false)
	hit(d)
	if n := len(l.kind("cmd")); n != 0 {
		t.Fatalf("louder sent %d cmd events", n)
	}
}

func TestOnLEDAudioRateLimited(t *testing.T) {
	d, l := newLEDTest(t, "", false)
	now := time.Unix(1000, 0)
	d.now = func() time.Time { return now }
	pcm := make([]int16, 320)
	for i := range pcm {
		pcm[i] = 1000
	}
	for i := 0; i < 100; i++ {
		d.onAudio(pcm)
		now = now.Add(5 * time.Millisecond)
	}
	n := len(l.kind("audio"))
	if n < 10 || n > 13 {
		t.Fatalf("%d audio events in 0.5 s, want 10..13", n)
	}
	e := l.kind("audio")[0]
	if len(e.Bands) != 12 || e.Level <= 0 || e.Level > 1 {
		t.Fatalf("bad audio event %+v", e)
	}
}
