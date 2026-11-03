// Package alarms keeps the speaker's alarms and timers: what is set, when it
// fires and whether it is ringing. It knows nothing of sound or the network;
// the clock and the ringing come from outside.
package alarms

import (
	"fmt"
	"time"
)

// Alarm fires at Hour:Minute: on Days, on Date, or the next time it comes.
type Alarm struct {
	ID     string `json:"id"`
	Hour   int    `json:"hour"`
	Minute int    `json:"minute"`
	Days   []int  `json:"days,omitempty"` // ISO weekdays, Monday = 1, Sunday = 7
	Date   string `json:"date,omitempty"` // "2006-01-02"; only without Days
	Title  string `json:"title,omitempty"`

	Disabled bool   `json:"disabled,omitempty"`
	Sound    string `json:"sound,omitempty"` // a name from Sounds; empty = the default
	Music    string `json:"music,omitempty"` // what to ask Spotify for instead of Sound

	SnoozeUntil time.Time `json:"snooze_until,omitzero"`
	Armed       time.Time `json:"armed,omitzero"` // set or switched on; nothing before it counts
	Fired       time.Time `json:"fired,omitzero"` // the firing that rang last, as scheduled
}

// Timer runs down to EndsAt, or holds Left while Paused.
type Timer struct {
	ID       string        `json:"id"`
	Title    string        `json:"title,omitempty"`
	Duration time.Duration `json:"duration"`
	EndsAt   time.Time     `json:"ends_at,omitzero"`
	Left     time.Duration `json:"left,omitempty"`
	Paused   bool          `json:"paused,omitempty"`
}

const dateLayout = "2006-01-02"

// Validate says what is wrong with the alarm, in the user's words.
func (a Alarm) Validate() error {
	if a.Hour < 0 || a.Hour > 23 || a.Minute < 0 || a.Minute > 59 {
		return fmt.Errorf("нет такого времени: %d:%02d", a.Hour, a.Minute)
	}
	for _, d := range a.Days {
		if d < 1 || d > 7 {
			return fmt.Errorf("нет такого дня недели: %d", d)
		}
	}
	if a.Date != "" {
		if len(a.Days) > 0 {
			return fmt.Errorf("будильник либо на дату, либо по дням недели")
		}
		if _, err := time.Parse(dateLayout, a.Date); err != nil {
			return fmt.Errorf("нет такой даты: %s", a.Date)
		}
	}
	return nil
}

// Next is the first firing later than after, in after's time zone, or the zero
// time if there is none. A snooze counts even on a disabled alarm: a finished
// one-off is disabled and can still be snoozed.
func (a Alarm) Next(after time.Time) time.Time {
	var next time.Time
	if !a.Disabled {
		next = a.scheduled(after)
	}
	if a.SnoozeUntil.After(after) && (next.IsZero() || a.SnoozeUntil.Before(next)) {
		return a.SnoozeUntil
	}
	return next
}

func (a Alarm) scheduled(after time.Time) time.Time {
	at := func(y int, m time.Month, d int) time.Time {
		return time.Date(y, m, d, a.Hour, a.Minute, 0, 0, after.Location())
	}
	if a.Date != "" {
		d, err := time.Parse(dateLayout, a.Date)
		if err != nil {
			return time.Time{}
		}
		if t := at(d.Year(), d.Month(), d.Day()); t.After(after) {
			return t
		}
		return time.Time{}
	}
	y, m, d := after.Date()
	// Today can be past already, so a weekly alarm may be a full week and a day
	// away; one more for a day the clocks took the time out of.
	for i := range 9 {
		t := at(y, m, d+i)
		if t.After(after) && a.onDay(t.Weekday()) {
			return t
		}
	}
	return time.Time{}
}

func (a Alarm) onDay(w time.Weekday) bool {
	if len(a.Days) == 0 {
		return true
	}
	iso := int(w)
	if iso == 0 {
		iso = 7
	}
	for _, d := range a.Days {
		if d == iso {
			return true
		}
	}
	return false
}
