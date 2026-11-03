package alarms

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

const (
	ringTimeout  = 5 * time.Minute // a ring nobody stops ends by itself
	missedLimit  = 5 * time.Minute // missed while voiced was down: rings only this late
	snoozeFor    = 10 * time.Minute
	snoozeWindow = 2 * time.Minute // "отложи" still means the alarm that just rang
	knobGuard    = time.Second     // one turn of the knob is several detents

	rampStart = 10
	rampStep  = 10
	rampEvery = 20 * time.Second
	rampEnd   = 70
)

// Kind is what rings.
type Kind string

const (
	KindAlarm Kind = "alarm"
	KindTimer Kind = "timer"
)

// Ring is one ring as the hardware needs it.
type Ring struct {
	Kind  Kind
	Sound string
	Music string
	Title string
}

// Ringer makes the sound; its calls must not block.
type Ringer interface {
	Start(r Ring)
	Stop(snoozed bool)
}

// Status is the ring and the store as the panel shows them.
type Status struct {
	Ring  Kind      `json:"ring,omitempty"`
	Title string    `json:"title,omitempty"`
	Since time.Time `json:"since,omitzero"`
	Error string    `json:"error,omitempty"`
}

// RingLevel is an alarm's volume this long into its ring.
func RingLevel(elapsed time.Duration) int {
	return min(rampStart+rampStep*int(elapsed/rampEvery), rampEnd)
}

// Manager keeps the alarms and timers and decides when to ring.
type Manager struct {
	// OnChange is told of any change the panel should see, without locks held.
	// Set it before Run.
	OnChange func()

	path   string
	ringer Ringer
	now    func() time.Time

	ringing atomic.Bool // read on the audio goroutine

	mu       sync.Mutex
	st       State
	err      string
	ring     Ring
	since    time.Time
	alarmID  string    // the alarm ringing or rung last
	stopped  time.Time // when the last ring ended
	knobOver time.Time
}

// New loads the state; a store it cannot read shows in Status.
func New(path string, r Ringer, now func() time.Time) *Manager {
	m := &Manager{path: path, ringer: r, now: now}
	st, err := Load(path)
	m.st = st
	if err != nil {
		log.Printf("alarms: %v", err)
		m.err = err.Error()
	}
	return m
}

// Run checks the clock until ctx ends. Only the wall clock counts: the wanted
// moment is compared on every tick, so nothing depends on sleeping accurately.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	m.Tick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Tick()
		}
	}
}

// Tick does what is due now.
func (m *Manager) Tick() {
	now := m.now()
	m.mu.Lock()
	var start *Ring
	var stop, changed bool
	if m.ringing.Load() && now.Sub(m.since) >= ringTimeout {
		m.endRingLocked(now)
		stop, changed = true, true
		log.Printf("alarms: nobody stopped the ring")
	}
	rings, dirty := m.dueLocked(now)
	if len(rings) > 0 && !m.ringing.Load() {
		r := rings[0]
		m.ring, m.since = r.Ring, now
		m.alarmID = r.alarmID
		m.ringing.Store(true)
		start = &r.Ring
		changed = true
	}
	if dirty {
		m.saveLocked()
		changed = true
	}
	m.mu.Unlock()
	if stop {
		m.ringer.Stop(false)
	}
	if start != nil {
		log.Printf("alarms: ring %s %q", start.Kind, start.Title)
		m.ringer.Start(*start)
	}
	if changed {
		m.changed()
	}
}

type due struct {
	Ring
	alarmID string
}

// dueLocked marks what has come as fired and returns what should ring, alarms
// first. It counts from the last firing, not from the previous tick, so a clock
// moved back cannot ring the same firing twice.
func (m *Manager) dueLocked(now time.Time) (rings []due, dirty bool) {
	for i := range m.st.Alarms {
		a := &m.st.Alarms[i]
		for {
			from := a.Armed
			if a.Fired.After(from) {
				from = a.Fired
			}
			next := a.Next(from.In(now.Location()))
			if next.IsZero() || next.After(now) {
				break
			}
			a.Fired, dirty = next, true
			snoozed := next.Equal(a.SnoozeUntil)
			if snoozed {
				a.SnoozeUntil = time.Time{}
			} else if len(a.Days) == 0 {
				a.Disabled = true
			}
			if now.Sub(next) > missedLimit {
				log.Printf("alarms: alarm %s missed at %v", a.ID, next)
				continue
			}
			rings = append(rings, due{Ring{KindAlarm, a.Sound, a.Music, a.Title}, a.ID})
		}
	}
	kept := m.st.Timers[:0]
	for _, t := range m.st.Timers {
		if t.Paused || t.EndsAt.After(now) {
			kept = append(kept, t)
			continue
		}
		dirty = true
		if now.Sub(t.EndsAt) > missedLimit {
			log.Printf("alarms: timer %s missed at %v", t.ID, t.EndsAt)
			continue
		}
		rings = append(rings, due{Ring: Ring{Kind: KindTimer, Sound: TimerSound, Title: t.Title}})
	}
	m.st.Timers = kept
	return rings, dirty
}

func (m *Manager) endRingLocked(now time.Time) {
	m.ringing.Store(false)
	m.stopped = now
	m.knobOver = now.Add(knobGuard)
}

// Ringing says whether a ring is on; it never blocks.
func (m *Manager) Ringing() bool { return m.ringing.Load() }

// ConsumesKnob says whether a knob detent belongs to the ring rather than the
// volume: during it, and for the rest of the turn that stopped it.
func (m *Manager) ConsumesKnob() bool {
	if m.ringing.Load() {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.now().Before(m.knobOver)
}

// Stop ends the ring, if there is one.
func (m *Manager) Stop() {
	m.mu.Lock()
	if !m.ringing.Load() {
		m.mu.Unlock()
		return
	}
	m.endRingLocked(m.now())
	m.mu.Unlock()
	m.ringer.Stop(false)
	m.changed()
}

// Snooze moves the alarm ringing or just stopped ahead and returns when it rings
// again.
func (m *Manager) Snooze() (time.Time, error) {
	now := m.now()
	m.mu.Lock()
	ringing := m.ringing.Load()
	if ringing && m.ring.Kind != KindAlarm {
		m.mu.Unlock()
		return time.Time{}, fmt.Errorf("таймер нельзя отложить")
	}
	if !ringing && (m.stopped.IsZero() || now.Sub(m.stopped) > snoozeWindow) || m.alarmID == "" {
		m.mu.Unlock()
		return time.Time{}, fmt.Errorf("сейчас нечего откладывать")
	}
	a := m.alarmLocked(m.alarmID)
	if a == nil {
		m.mu.Unlock()
		return time.Time{}, fmt.Errorf("этого будильника уже нет")
	}
	until := now.Add(snoozeFor)
	a.SnoozeUntil = until
	if ringing {
		m.endRingLocked(now)
	}
	m.saveLocked()
	m.mu.Unlock()
	if ringing {
		m.ringer.Stop(true)
	}
	m.changed()
	return until, nil
}

// Test rings as an alarm with the default sound, for the panel.
func (m *Manager) Test() {
	m.mu.Lock()
	if m.ringing.Load() {
		m.mu.Unlock()
		return
	}
	m.ring, m.since = Ring{Kind: KindAlarm}, m.now()
	m.alarmID = ""
	m.ringing.Store(true)
	r := m.ring
	m.mu.Unlock()
	m.ringer.Start(r)
	m.changed()
}

// Status is the ring and the store's last error.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{Error: m.err}
	if m.ringing.Load() {
		s.Ring, s.Title, s.Since = m.ring.Kind, m.ring.Title, m.since
	}
	return s
}

// Alarms returns a copy of the alarms.
func (m *Manager) Alarms() []Alarm {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Alarm, len(m.st.Alarms))
	copy(out, m.st.Alarms)
	return out
}

// AddAlarm sets a new alarm, counted from now.
func (m *Manager) AddAlarm(a Alarm) (Alarm, error) {
	if err := a.Validate(); err != nil {
		return Alarm{}, err
	}
	a.ID, a.Armed, a.Fired, a.SnoozeUntil = newID(), m.now(), time.Time{}, time.Time{}
	m.mu.Lock()
	m.st.Alarms = append(m.st.Alarms, a)
	m.saveLocked()
	m.mu.Unlock()
	m.changed()
	return a, nil
}

// UpdateAlarm replaces the alarm of a.ID. An edited alarm starts afresh: from
// now, with no snooze left.
func (m *Manager) UpdateAlarm(a Alarm) error {
	if err := a.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	cur := m.alarmLocked(a.ID)
	if cur == nil {
		m.mu.Unlock()
		return fmt.Errorf("нет такого будильника")
	}
	a.Armed, a.Fired, a.SnoozeUntil = m.now(), time.Time{}, time.Time{}
	*cur = a
	m.saveLocked()
	m.mu.Unlock()
	m.changed()
	return nil
}

// DeleteAlarm removes the alarm.
func (m *Manager) DeleteAlarm(id string) error {
	m.mu.Lock()
	for i := range m.st.Alarms {
		if m.st.Alarms[i].ID == id {
			m.st.Alarms = append(m.st.Alarms[:i], m.st.Alarms[i+1:]...)
			m.saveLocked()
			m.mu.Unlock()
			m.changed()
			return nil
		}
	}
	m.mu.Unlock()
	return fmt.Errorf("нет такого будильника")
}

func (m *Manager) alarmLocked(id string) *Alarm {
	for i := range m.st.Alarms {
		if m.st.Alarms[i].ID == id {
			return &m.st.Alarms[i]
		}
	}
	return nil
}

// Timers returns a copy of the timers still to ring.
func (m *Manager) Timers() []Timer {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Timer, len(m.st.Timers))
	copy(out, m.st.Timers)
	return out
}

// Remaining is the time left at now.
func (t Timer) Remaining(now time.Time) time.Duration {
	if t.Paused {
		return t.Left
	}
	return max(t.EndsAt.Sub(now), 0)
}

// AddTimer starts a timer.
func (m *Manager) AddTimer(d time.Duration, title string) (Timer, error) {
	if d <= 0 {
		return Timer{}, fmt.Errorf("таймер должен быть длиннее нуля")
	}
	t := Timer{ID: newID(), Title: title, Duration: d, EndsAt: m.now().Add(d)}
	m.mu.Lock()
	m.st.Timers = append(m.st.Timers, t)
	m.saveLocked()
	m.mu.Unlock()
	m.changed()
	return t, nil
}

// PauseTimer holds what is left of the timer.
func (m *Manager) PauseTimer(id string) error {
	return m.editTimer(id, func(t *Timer, now time.Time) {
		if !t.Paused {
			t.Left, t.Paused = t.Remaining(now), true
		}
	})
}

// ResumeTimer lets the timer run again.
func (m *Manager) ResumeTimer(id string) error {
	return m.editTimer(id, func(t *Timer, now time.Time) {
		if t.Paused {
			t.EndsAt, t.Left, t.Paused = now.Add(t.Left), 0, false
		}
	})
}

func (m *Manager) editTimer(id string, f func(t *Timer, now time.Time)) error {
	m.mu.Lock()
	for i := range m.st.Timers {
		if m.st.Timers[i].ID == id {
			f(&m.st.Timers[i], m.now())
			m.saveLocked()
			m.mu.Unlock()
			m.changed()
			return nil
		}
	}
	m.mu.Unlock()
	return fmt.Errorf("нет такого таймера")
}

// CancelTimer removes the timer.
func (m *Manager) CancelTimer(id string) error {
	m.mu.Lock()
	for i := range m.st.Timers {
		if m.st.Timers[i].ID == id {
			m.st.Timers = append(m.st.Timers[:i], m.st.Timers[i+1:]...)
			m.saveLocked()
			m.mu.Unlock()
			m.changed()
			return nil
		}
	}
	m.mu.Unlock()
	return fmt.Errorf("нет такого таймера")
}

// saveLocked writes the state. A failure leaves the state right in memory: the
// alarm still rings unless voiced restarts.
func (m *Manager) saveLocked() {
	if err := Save(m.path, m.st); err != nil {
		log.Printf("alarms: %v", err)
		m.err = err.Error()
		return
	}
	m.err = ""
}

func (m *Manager) changed() {
	if m.OnChange != nil {
		m.OnChange()
	}
}

func newID() string {
	var b [4]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
