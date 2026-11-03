package daemon

import (
	"context"
	"net/http"
	"time"

	"voice/internal/alarms"
)

// AlarmsControl is alarms.Manager as the panel uses it.
type AlarmsControl interface {
	Alarms() []alarms.Alarm
	AddAlarm(alarms.Alarm) (alarms.Alarm, error)
	UpdateAlarm(alarms.Alarm) error
	DeleteAlarm(id string) error
	Timers() []alarms.Timer
	AddTimer(d time.Duration, title string) (alarms.Timer, error)
	PauseTimer(id string) error
	ResumeTimer(id string) error
	CancelTimer(id string) error
	Status() alarms.Status
	Stop()
	Snooze() (time.Time, error)
	Test()
}

// alarmView is an alarm and when it rings next.
type alarmView struct {
	alarms.Alarm
	Next time.Time `json:"next,omitzero"`
}

// timerView gives seconds. EndsAt lets a page count down by itself, so the state
// does not change each second.
type timerView struct {
	ID      string    `json:"id"`
	Title   string    `json:"title,omitempty"`
	Seconds int       `json:"seconds"`
	Left    int       `json:"left"`
	EndsAt  time.Time `json:"ends_at,omitzero"`
	Paused  bool      `json:"paused,omitempty"`
}

// alarmsView is the panel's picture. Left is only in the replies to a request:
// here it would change the state each second.
type alarmsView struct {
	Alarms []alarmView   `json:"alarms"`
	Timers []timerView   `json:"timers"`
	Ring   alarms.Status `json:"ring"`
}

func viewAlarm(a alarms.Alarm, now time.Time) alarmView { return alarmView{a, a.Next(now)} }

func viewTimer(t alarms.Timer, now time.Time) timerView {
	v := timerView{ID: t.ID, Title: t.Title, Seconds: int(t.Duration / time.Second), Paused: t.Paused,
		Left: int(t.Remaining(now).Round(time.Second) / time.Second)}
	if !t.Paused {
		v.EndsAt = t.EndsAt
	}
	return v
}

func (d *Daemon) alarmsView() *alarmsView {
	c := d.opt.Alarms
	if c == nil {
		return nil
	}
	now := time.Now()
	v := &alarmsView{Alarms: []alarmView{}, Timers: []timerView{}, Ring: c.Status()}
	for _, a := range c.Alarms() {
		v.Alarms = append(v.Alarms, viewAlarm(a, now))
	}
	for _, t := range c.Timers() {
		tv := viewTimer(t, now)
		if !t.Paused {
			tv.Left = 0
		}
		v.Timers = append(v.Timers, tv)
	}
	return v
}

// WatchAlarms has the panel told at once of a change. It only pokes the hub: the
// ring's stop runs under d.mu.
func (d *Daemon) WatchAlarms(m *alarms.Manager) { m.SetOnChange(d.hub.poke) }

func (d *Daemon) alarmRoutes(api func(string, func(http.ResponseWriter, *http.Request) error)) {
	c := d.opt.Alarms
	if c == nil {
		return
	}
	// alarmBody is what a request may set; the rest stays the manager's.
	type alarmBody struct {
		Hour     *int    `json:"hour"`
		Minute   *int    `json:"minute"`
		Days     *[]int  `json:"days"`
		Date     *string `json:"date"`
		Title    *string `json:"title"`
		Disabled *bool   `json:"disabled"`
		Sound    *string `json:"sound"`
		Music    *string `json:"music"`
		Kind     *string `json:"music_kind"`
	}
	// chooseMusic finds the music if the request named any, and says when it
	// could not: the alarm is kept, to search when it rings.
	chooseMusic := func(ctx context.Context, a *alarms.Alarm, b alarmBody) string {
		if b.Music == nil && b.Kind == nil {
			return ""
		}
		a.MusicItem = nil
		kind := ""
		if b.Kind != nil {
			kind = *b.Kind
		}
		if d.opt.FindMusic == nil || (a.Music == "" && kind != "liked") {
			return ""
		}
		m, err := d.opt.FindMusic(ctx, a.Music, kind)
		if err != nil {
			return "Музыку сейчас найти не удалось, будильник поищет её перед звонком: " + err.Error()
		}
		a.MusicItem = &m
		return ""
	}
	apply := func(a *alarms.Alarm, b alarmBody) error {
		set := func(dst *int, v *int) {
			if v != nil {
				*dst = *v
			}
		}
		text := func(dst *string, v *string) {
			if v != nil {
				*dst = *v
			}
		}
		set(&a.Hour, b.Hour)
		set(&a.Minute, b.Minute)
		text(&a.Date, b.Date)
		text(&a.Title, b.Title)
		text(&a.Sound, b.Sound)
		text(&a.Music, b.Music)
		if b.Days != nil {
			a.Days = *b.Days
		}
		if b.Disabled != nil {
			a.Disabled = *b.Disabled
		}
		if a.Sound != "" && !alarms.IsSound(a.Sound) {
			return badRequest("нет такой мелодии")
		}
		if err := a.Validate(); err != nil {
			return badRequest(err.Error())
		}
		return nil
	}
	find := func(id string) *alarms.Alarm {
		for _, a := range c.Alarms() {
			if a.ID == id {
				return &a
			}
		}
		return nil
	}
	api("GET /alarms", func(w http.ResponseWriter, r *http.Request) error {
		writeJSON(w, d.alarmsView())
		return nil
	})
	api("GET /alarms/sounds", func(w http.ResponseWriter, r *http.Request) error {
		writeJSON(w, map[string]any{"sounds": alarms.Sounds})
		return nil
	})
	api("POST /alarms", func(w http.ResponseWriter, r *http.Request) error {
		var b alarmBody
		if err := decode(r, &b); err != nil {
			return badRequest("неверный запрос: " + err.Error())
		}
		var a alarms.Alarm
		if err := apply(&a, b); err != nil {
			return err
		}
		note := chooseMusic(r.Context(), &a, b)
		a, err := c.AddAlarm(a)
		if err != nil {
			return badRequest(err.Error())
		}
		writeJSON(w, struct {
			alarmView
			Note string `json:"music_note,omitempty"`
		}{viewAlarm(a, time.Now()), note})
		return nil
	})
	api("PATCH /alarms/{id}", func(w http.ResponseWriter, r *http.Request) error {
		var b alarmBody
		if err := decode(r, &b); err != nil {
			return badRequest("неверный запрос: " + err.Error())
		}
		a := find(r.PathValue("id"))
		if a == nil {
			return notFound("нет такого будильника")
		}
		if err := apply(a, b); err != nil {
			return err
		}
		note := chooseMusic(r.Context(), a, b)
		if err := c.UpdateAlarm(*a); err != nil {
			return notFound(err.Error())
		}
		writeJSON(w, map[string]any{"ok": true, "music_note": note})
		return nil
	})
	api("DELETE /alarms/{id}", func(w http.ResponseWriter, r *http.Request) error {
		if err := c.DeleteAlarm(r.PathValue("id")); err != nil {
			return notFound(err.Error())
		}
		writeOK(w)
		return nil
	})
	api("GET /timers", func(w http.ResponseWriter, r *http.Request) error {
		now := time.Now()
		out := []timerView{}
		for _, t := range c.Timers() {
			out = append(out, viewTimer(t, now))
		}
		writeJSON(w, map[string]any{"timers": out})
		return nil
	})
	api("POST /timers", func(w http.ResponseWriter, r *http.Request) error {
		var b struct {
			Seconds int    `json:"seconds"`
			Title   string `json:"title"`
		}
		if err := decode(r, &b); err != nil {
			return badRequest("неверный запрос: " + err.Error())
		}
		t, err := c.AddTimer(time.Duration(b.Seconds)*time.Second, b.Title)
		if err != nil {
			return badRequest(err.Error())
		}
		writeJSON(w, viewTimer(t, time.Now()))
		return nil
	})
	api("PATCH /timers/{id}", func(w http.ResponseWriter, r *http.Request) error {
		var b struct {
			Paused *bool `json:"paused"`
		}
		if err := decode(r, &b); err != nil {
			return badRequest("неверный запрос: " + err.Error())
		}
		if b.Paused == nil {
			return badRequest("нужно поле paused")
		}
		edit := c.ResumeTimer
		if *b.Paused {
			edit = c.PauseTimer
		}
		if err := edit(r.PathValue("id")); err != nil {
			return notFound(err.Error())
		}
		writeOK(w)
		return nil
	})
	api("DELETE /timers/{id}", func(w http.ResponseWriter, r *http.Request) error {
		if err := c.CancelTimer(r.PathValue("id")); err != nil {
			return notFound(err.Error())
		}
		writeOK(w)
		return nil
	})
	api("POST /ring/stop", func(w http.ResponseWriter, r *http.Request) error {
		c.Stop()
		writeOK(w)
		return nil
	})
	api("POST /ring/snooze", func(w http.ResponseWriter, r *http.Request) error {
		until, err := c.Snooze()
		if err != nil {
			return conflict(err.Error())
		}
		writeJSON(w, map[string]any{"until": until})
		return nil
	})
	api("POST /ring/test", func(w http.ResponseWriter, r *http.Request) error {
		c.Test()
		writeOK(w)
		return nil
	})
}
