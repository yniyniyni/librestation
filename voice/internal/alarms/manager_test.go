package alarms

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeRinger records what the manager asks of the hardware.
type fakeRinger struct {
	mu     sync.Mutex
	starts []Ring
	stops  []bool
}

func (f *fakeRinger) Start(r Ring) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, r)
}

func (f *fakeRinger) Stop(snoozed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, snoozed)
}

type rig struct {
	t    *testing.T
	m    *Manager
	r    *fakeRinger
	path string
	now  time.Time
}

func newRig(t *testing.T) *rig {
	t.Helper()
	g := &rig{t: t, r: &fakeRinger{}, path: filepath.Join(t.TempDir(), "alarms.json"),
		now: time.Date(2026, 11, 6, 6, 59, 0, 0, time.UTC)} // a Friday
	g.m = New(g.path, g.r, func() time.Time { return g.now })
	return g
}

func (g *rig) advance(d time.Duration) {
	g.now = g.now.Add(d)
	g.m.Tick()
}

func (g *rig) addAlarm(a Alarm) Alarm {
	g.t.Helper()
	a, err := g.m.AddAlarm(a)
	if err != nil {
		g.t.Fatal(err)
	}
	return a
}

func (g *rig) wantRings(n int) {
	g.t.Helper()
	if len(g.r.starts) != n {
		g.t.Fatalf("%d rings, want %d: %+v", len(g.r.starts), n, g.r.starts)
	}
}

func TestAlarmRingsOnTime(t *testing.T) {
	g := newRig(t)
	id := g.addAlarm(Alarm{Hour: 7, Sound: "birds", Title: "работа"}).ID
	g.advance(59 * time.Second)
	g.wantRings(0)
	g.advance(time.Second)
	g.wantRings(1)
	if r := g.r.starts[0]; r.Kind != KindAlarm || r.Sound != "birds" || r.Title != "работа" {
		t.Errorf("ring %+v", r)
	}
	if !g.m.Ringing() {
		t.Error("not ringing")
	}
	if a := g.m.Alarms()[0]; a.ID != id || !a.Disabled {
		t.Errorf("a one-off stays on after it rang: %+v", a)
	}
}

func TestRepeatingStaysOn(t *testing.T) {
	g := newRig(t)
	g.addAlarm(Alarm{Hour: 7, Days: []int{5, 6}})
	g.advance(time.Minute)
	g.wantRings(1)
	g.m.Stop()
	if g.m.Alarms()[0].Disabled {
		t.Error("a repeating alarm went off for good")
	}
	g.advance(24 * time.Hour)
	g.wantRings(2)
}

func TestClockJump(t *testing.T) {
	g := newRig(t)
	g.addAlarm(Alarm{Hour: 7, Days: []int{5}})
	g.advance(time.Minute)
	g.wantRings(1)
	g.m.Stop()
	g.advance(-time.Hour) // back across the firing
	g.advance(time.Hour)
	g.advance(2 * time.Second)
	g.wantRings(1)
}

func TestSimultaneous(t *testing.T) {
	g := newRig(t)
	if _, err := g.m.AddTimer(time.Minute, "чай"); err != nil {
		t.Fatal(err)
	}
	g.addAlarm(Alarm{Hour: 7, Sound: "kids"})
	g.addAlarm(Alarm{Hour: 7, Days: []int{5}})
	g.advance(time.Minute)
	g.wantRings(1)
	if g.r.starts[0].Kind != KindAlarm || g.r.starts[0].Sound != "kids" {
		t.Errorf("ring %+v", g.r.starts[0])
	}
	g.m.Stop()
	if len(g.r.stops) != 1 || g.m.Ringing() {
		t.Errorf("stops %v, ringing %v", g.r.stops, g.m.Ringing())
	}
	if len(g.m.Timers()) != 0 {
		t.Errorf("timer left: %+v", g.m.Timers())
	}
	g.advance(time.Second)
	g.wantRings(1)
}

func TestFiringDuringRingJoinsIt(t *testing.T) {
	g := newRig(t)
	g.addAlarm(Alarm{Hour: 7})
	g.addAlarm(Alarm{Hour: 7, Minute: 1, Days: []int{5}})
	g.advance(time.Minute)
	g.wantRings(1)
	g.advance(time.Minute)
	g.wantRings(1)
	g.m.Stop()
	g.advance(time.Minute)
	g.wantRings(1)
}

func TestRingTimesOut(t *testing.T) {
	g := newRig(t)
	g.addAlarm(Alarm{Hour: 7})
	g.advance(time.Minute)
	g.advance(ringTimeout - time.Second)
	if !g.m.Ringing() {
		t.Fatal("stopped early")
	}
	g.advance(time.Second)
	if g.m.Ringing() || len(g.r.stops) != 1 || g.r.stops[0] {
		t.Errorf("ringing %v, stops %v", g.m.Ringing(), g.r.stops)
	}
}

func TestMissedWhileDown(t *testing.T) {
	cases := []struct {
		name  string
		a     Alarm
		since time.Duration
		rings int
		off   bool
	}{
		{"just missed", Alarm{Hour: 7}, 4 * time.Minute, 1, true},
		{"long ago", Alarm{Hour: 7}, 6 * time.Minute, 0, true},
		{"long ago, repeating", Alarm{Hour: 7, Days: []int{5}}, 6 * time.Minute, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := newRig(t)
			g.addAlarm(c.a)
			g.now = time.Date(2026, 11, 6, 7, 0, 0, 0, time.UTC).Add(c.since)
			m := New(g.path, g.r, func() time.Time { return g.now })
			m.Tick()
			g.wantRings(c.rings)
			if a := m.Alarms()[0]; a.Disabled != c.off {
				t.Errorf("%+v", a)
			}
		})
	}
}

func TestMissedTimer(t *testing.T) {
	for _, c := range []struct {
		since time.Duration
		rings int
	}{{4 * time.Minute, 1}, {6 * time.Minute, 0}} {
		g := newRig(t)
		if _, err := g.m.AddTimer(time.Minute, ""); err != nil {
			t.Fatal(err)
		}
		g.now = g.now.Add(time.Minute + c.since)
		m := New(g.path, g.r, func() time.Time { return g.now })
		m.Tick()
		g.wantRings(c.rings)
		if len(m.Timers()) != 0 {
			t.Errorf("timer left: %+v", m.Timers())
		}
	}
}

func TestTimerPauseAndResume(t *testing.T) {
	g := newRig(t)
	tm, err := g.m.AddTimer(time.Minute, "чай")
	if err != nil {
		t.Fatal(err)
	}
	g.advance(20 * time.Second)
	if err := g.m.PauseTimer(tm.ID); err != nil {
		t.Fatal(err)
	}
	if got := g.m.Timers()[0]; !got.Paused || got.Left != 40*time.Second || got.Remaining(g.now) != 40*time.Second {
		t.Errorf("paused: %+v", got)
	}
	g.advance(time.Hour)
	g.wantRings(0)
	if err := g.m.ResumeTimer(tm.ID); err != nil {
		t.Fatal(err)
	}
	g.advance(39 * time.Second)
	g.wantRings(0)
	g.advance(time.Second)
	g.wantRings(1)
	if r := g.r.starts[0]; r.Kind != KindTimer || r.Sound != TimerSound || r.Title != "чай" {
		t.Errorf("ring %+v", r)
	}
}

func TestTimerSurvivesRestart(t *testing.T) {
	g := newRig(t)
	if _, err := g.m.AddTimer(time.Minute, ""); err != nil {
		t.Fatal(err)
	}
	g.now = g.now.Add(30 * time.Second)
	m := New(g.path, g.r, func() time.Time { return g.now })
	m.Tick()
	g.wantRings(0)
	if left := m.Timers()[0].Remaining(g.now); left != 30*time.Second {
		t.Errorf("left %v", left)
	}
}

func TestBadInput(t *testing.T) {
	g := newRig(t)
	if _, err := g.m.AddTimer(0, ""); err == nil {
		t.Error("a timer of nothing")
	}
	if _, err := g.m.AddAlarm(Alarm{Hour: 25}); err == nil {
		t.Error("an alarm at 25:00")
	}
	for name, err := range map[string]error{
		"pause": g.m.PauseTimer("x"), "resume": g.m.ResumeTimer("x"), "cancel": g.m.CancelTimer("x"),
		"delete": g.m.DeleteAlarm("x"), "update": g.m.UpdateAlarm(Alarm{ID: "x"}),
	} {
		if err == nil {
			t.Errorf("%s of nothing passed", name)
		}
	}
}

func TestSwitchingBackOnCountsFromNow(t *testing.T) {
	g := newRig(t)
	a := g.addAlarm(Alarm{Hour: 7})
	a.Disabled = true
	if err := g.m.UpdateAlarm(a); err != nil {
		t.Fatal(err)
	}
	g.advance(2 * time.Minute) // past 7:00
	g.wantRings(0)
	a.Disabled = false
	if err := g.m.UpdateAlarm(a); err != nil {
		t.Fatal(err)
	}
	g.advance(time.Minute)
	g.wantRings(0) // not this morning's 7:00, still within the missed limit
	g.advance(24 * time.Hour)
	g.wantRings(1)
}

func TestSnoozeWhileRinging(t *testing.T) {
	g := newRig(t)
	g.addAlarm(Alarm{Hour: 7})
	g.advance(time.Minute)
	until, err := g.m.Snooze()
	if err != nil {
		t.Fatal(err)
	}
	if want := g.now.Add(snoozeFor); !until.Equal(want) {
		t.Errorf("until %v, want %v", until, want)
	}
	if g.m.Ringing() || len(g.r.stops) != 1 || !g.r.stops[0] {
		t.Errorf("ringing %v, stops %v", g.m.Ringing(), g.r.stops)
	}
	g.advance(snoozeFor - time.Second)
	g.wantRings(1)
	g.advance(time.Second)
	g.wantRings(2)
	g.m.Stop()
	g.advance(24 * time.Hour)
	g.wantRings(2) // the one-off is over
}

// The trigger word stops the ring and only then is "отложи" heard.
func TestSnoozeJustAfterStop(t *testing.T) {
	g := newRig(t)
	g.addAlarm(Alarm{Hour: 7})
	g.advance(time.Minute)
	g.m.Stop()
	g.advance(snoozeWindow)
	if _, err := g.m.Snooze(); err != nil {
		t.Fatal(err)
	}
	g.advance(snoozeFor)
	g.wantRings(2)
}

func TestSnoozeRefused(t *testing.T) {
	g := newRig(t)
	if _, err := g.m.Snooze(); err == nil {
		t.Error("nothing rang")
	}
	g.addAlarm(Alarm{Hour: 7})
	g.advance(time.Minute)
	g.m.Stop()
	g.advance(snoozeWindow + time.Second)
	if _, err := g.m.Snooze(); err == nil {
		t.Error("rang too long ago")
	}
	if _, err := g.m.AddTimer(time.Minute, ""); err != nil {
		t.Fatal(err)
	}
	g.advance(time.Minute)
	if _, err := g.m.Snooze(); err == nil {
		t.Error("a timer snoozed")
	}
	if !g.m.Ringing() {
		t.Error("a refused snooze stopped the ring")
	}
}

func TestConsumesKnob(t *testing.T) {
	g := newRig(t)
	if g.m.ConsumesKnob() {
		t.Error("nothing rings")
	}
	g.addAlarm(Alarm{Hour: 7})
	g.advance(time.Minute)
	if !g.m.ConsumesKnob() {
		t.Error("ringing")
	}
	g.m.Stop()
	g.now = g.now.Add(knobGuard - time.Millisecond)
	if !g.m.ConsumesKnob() {
		t.Error("the same turn's next detent")
	}
	g.now = g.now.Add(time.Millisecond)
	if g.m.ConsumesKnob() {
		t.Error("guard outlasted")
	}
}

func TestStopWithoutRing(t *testing.T) {
	g := newRig(t)
	g.m.Stop()
	if len(g.r.stops) != 0 {
		t.Errorf("stops %v", g.r.stops)
	}
}

func TestTestRing(t *testing.T) {
	g := newRig(t)
	g.m.Test()
	g.wantRings(1)
	if g.r.starts[0].Kind != KindAlarm {
		t.Errorf("%+v", g.r.starts[0])
	}
	if _, err := g.m.Snooze(); err == nil {
		t.Error("a test ring snoozed")
	}
	g.m.Stop()
}

func TestBrokenFileReported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "alarms.json")
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 6, 6, 59, 0, 0, time.UTC)
	m := New(path, &fakeRinger{}, func() time.Time { return now })
	if m.Status().Error == "" {
		t.Error("no error shown")
	}
	if _, err := m.AddAlarm(Alarm{Hour: 7}); err != nil {
		t.Fatal(err)
	}
	if s, err := Load(path); err != nil || len(s.Alarms) != 1 {
		t.Errorf("%+v, %v", s, err)
	}
}

func TestChangeReported(t *testing.T) {
	g := newRig(t)
	n := 0
	g.m.OnChange = func() { n++ }
	g.addAlarm(Alarm{Hour: 7})
	if n != 1 {
		t.Errorf("after add: %d", n)
	}
	g.advance(time.Minute)
	if n != 2 {
		t.Errorf("after the ring began: %d", n)
	}
	g.m.Stop()
	if n != 3 {
		t.Errorf("after stop: %d", n)
	}
	g.advance(time.Second)
	if n != 3 {
		t.Errorf("a quiet tick: %d", n)
	}
}

func TestRingLevel(t *testing.T) {
	for _, c := range []struct {
		at   time.Duration
		want int
	}{{0, 10}, {19 * time.Second, 10}, {20 * time.Second, 20}, {100 * time.Second, 60}, {120 * time.Second, 70}, {300 * time.Second, 70}} {
		if got := RingLevel(c.at); got != c.want {
			t.Errorf("RingLevel(%v) = %d, want %d", c.at, got, c.want)
		}
	}
}
