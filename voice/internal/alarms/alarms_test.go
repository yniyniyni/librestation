package alarms

import (
	"testing"
	"time"
)

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	l, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestNext(t *testing.T) {
	msk := zone(t, "Europe/Moscow")
	at := func(y int, m time.Month, d, h, min int) time.Time { return time.Date(y, m, d, h, min, 0, 0, msk) }
	// 2026-11-06 is a Friday.
	cases := []struct {
		name  string
		a     Alarm
		after time.Time
		want  time.Time
	}{
		{"once, today", Alarm{Hour: 7}, at(2026, 11, 6, 6, 59), at(2026, 11, 6, 7, 0)},
		{"once, tomorrow", Alarm{Hour: 7}, at(2026, 11, 6, 7, 1), at(2026, 11, 7, 7, 0)},
		{"exactly then is not after", Alarm{Hour: 7}, at(2026, 11, 6, 7, 0), at(2026, 11, 7, 7, 0)},
		{"weekdays skip the weekend", Alarm{Hour: 7, Minute: 30, Days: []int{1, 2, 3, 4, 5}}, at(2026, 11, 6, 20, 0), at(2026, 11, 9, 7, 30)},
		{"sunday is 7", Alarm{Hour: 9, Days: []int{7}}, at(2026, 11, 6, 20, 0), at(2026, 11, 8, 9, 0)},
		{"same weekday next week", Alarm{Hour: 7, Days: []int{5}}, at(2026, 11, 6, 8, 0), at(2026, 11, 13, 7, 0)},
		{"date", Alarm{Hour: 8, Date: "2026-11-20"}, at(2026, 11, 6, 8, 0), at(2026, 11, 20, 8, 0)},
		{"date passed", Alarm{Hour: 8, Date: "2026-11-01"}, at(2026, 11, 6, 8, 0), time.Time{}},
		{"month end", Alarm{Hour: 7}, at(2026, 11, 30, 8, 0), at(2026, 12, 1, 7, 0)},
		{"leap day", Alarm{Hour: 7, Date: "2028-02-29"}, at(2028, 2, 28, 8, 0), at(2028, 2, 29, 7, 0)},
		{"off", Alarm{Hour: 7, Disabled: true}, at(2026, 11, 6, 6, 0), time.Time{}},
		{"snooze before the schedule", Alarm{Hour: 7, Days: []int{5, 6}, SnoozeUntil: at(2026, 11, 6, 7, 10)}, at(2026, 11, 6, 7, 1), at(2026, 11, 6, 7, 10)},
		{"snooze of a finished one-off", Alarm{Hour: 7, Disabled: true, SnoozeUntil: at(2026, 11, 6, 7, 10)}, at(2026, 11, 6, 7, 1), at(2026, 11, 6, 7, 10)},
		{"schedule before a later snooze", Alarm{Hour: 7, Days: []int{5, 6}, SnoozeUntil: at(2026, 11, 7, 7, 10)}, at(2026, 11, 6, 20, 0), at(2026, 11, 7, 7, 0)},
		{"snooze passed", Alarm{Hour: 7, Days: []int{6}, SnoozeUntil: at(2026, 11, 6, 7, 10)}, at(2026, 11, 6, 7, 11), at(2026, 11, 7, 7, 0)},
	}
	for _, c := range cases {
		if got := c.a.Next(c.after); !got.Equal(c.want) {
			t.Errorf("%s: Next = %v, want %v", c.name, got, c.want)
		}
	}
}

// The clocks go forward over 02:30 here: the alarm still comes, once, later than
// the time asked about.
func TestNextOverClockChange(t *testing.T) {
	prg := zone(t, "Europe/Prague")
	after := time.Date(2026, 3, 29, 1, 0, 0, 0, prg)
	got := Alarm{Hour: 2, Minute: 30}.Next(after)
	if !got.After(after) || got.After(after.Add(26*time.Hour)) {
		t.Errorf("Next = %v", got)
	}
}

func TestValidate(t *testing.T) {
	good := []Alarm{{Hour: 23, Minute: 59}, {Days: []int{1, 7}}, {Date: "2026-11-20"}}
	for _, a := range good {
		if err := a.Validate(); err != nil {
			t.Errorf("%+v: %v", a, err)
		}
	}
	bad := []Alarm{{Hour: 24}, {Minute: 60}, {Hour: -1}, {Days: []int{0}}, {Days: []int{8}}, {Date: "завтра"},
		{Date: "2026-11-20", Days: []int{1}}}
	for _, a := range bad {
		if a.Validate() == nil {
			t.Errorf("%+v passed", a)
		}
	}
}
