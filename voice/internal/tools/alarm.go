package tools

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"voice/internal/agent"
	"voice/internal/alarms"
)

// isoDays are the names the model gives days by.
var isoDays = map[string][]int{
	"mon": {1}, "tue": {2}, "wed": {3}, "thu": {4}, "fri": {5}, "sat": {6}, "sun": {7},
	"weekdays": {1, 2, 3, 4, 5}, "weekends": {6, 7},
}

// onDays are the Russian "on that day" forms, by time.Weekday.
var onDays = [...]string{"в воскресенье", "в понедельник", "во вторник", "в среду", "в четверг", "в пятницу", "в субботу"}

// Alarm sets and changes alarms.
type Alarm struct {
	M   *alarms.Manager
	Now func() time.Time // nil = time.Now
}

func (a Alarm) now() time.Time { return orNow(a.Now) }

func orNow(now func() time.Time) time.Time {
	if now != nil {
		return now()
	}
	return time.Now()
}

func (Alarm) Decl() agent.FuncDecl {
	days := []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun", "weekdays", "weekends"}
	sounds := make([]string, 0, len(alarms.Sounds))
	for _, s := range alarms.Sounds {
		sounds = append(sounds, s.Name)
	}
	return agent.FuncDecl{
		Name: "alarm",
		Description: "Set or change alarms on this speaker. \"на 7\" means the next 7:00; do not ask, say the result. " +
			"Without days and date the alarm rings once. music is what to ask Spotify for instead of a ringtone. " +
			"snooze moves the alarm that just rang ten minutes ahead: \"отложи\". " +
			"enable, disable and delete find the alarm by time, or by id from alarms_list; a lone alarm needs neither.",
		Params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{"type": "string", "enum": []string{"set", "enable", "disable", "delete", "snooze"}},
				"time":   map[string]any{"type": "string", "description": "HH:MM, 24 hours"},
				"days":   map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": days}},
				"date":   map[string]any{"type": "string", "description": "YYYY-MM-DD, for one date"},
				"title":  map[string]any{"type": "string"},
				"sound":  map[string]any{"type": "string", "enum": sounds},
				"music":  map[string]any{"type": "string"},
				"id":     map[string]any{"type": "string"},
			},
			"required": []string{"action"},
		},
	}
}

func (a Alarm) Run(_ context.Context, args map[string]any) (map[string]any, error) {
	action, _ := args["action"].(string)
	switch action {
	case "set":
		return a.set(args)
	case "snooze":
		until, err := a.M.Snooze()
		if err != nil {
			return nil, err
		}
		return map[string]any{"when": whenWords(a.now(), until)}, nil
	case "enable", "disable":
		cur, err := a.find(args)
		if err != nil {
			return nil, err
		}
		cur.Disabled = action == "disable"
		if err := a.M.UpdateAlarm(cur); err != nil {
			return nil, err
		}
		res := map[string]any{"ok": true}
		if !cur.Disabled {
			res["when"] = whenWords(a.now(), cur.Next(a.now()))
		}
		return res, nil
	case "delete":
		cur, err := a.find(args)
		if err != nil {
			return nil, err
		}
		if err := a.M.DeleteAlarm(cur.ID); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	}
	return nil, fmt.Errorf("unknown action %q", action)
}

func (a Alarm) set(args map[string]any) (map[string]any, error) {
	clock, _ := args["time"].(string)
	h, m, err := parseClock(clock)
	if err != nil {
		return nil, err
	}
	al := alarms.Alarm{Hour: h, Minute: m}
	al.Date, _ = args["date"].(string)
	al.Title, _ = args["title"].(string)
	al.Sound, _ = args["sound"].(string)
	al.Music, _ = args["music"].(string)
	if al.Days, err = parseDays(args["days"]); err != nil {
		return nil, err
	}
	if al.Sound != "" && !alarms.IsSound(al.Sound) {
		return nil, fmt.Errorf("нет такой мелодии: %s", al.Sound)
	}
	al, err = a.M.AddAlarm(al)
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": al.ID, "when": whenWords(a.now(), al.Next(a.now()))}, nil
}

// find is the alarm the call means: by id, by time, or the only one there is.
// With several and no way to tell, the error names them for the model to ask.
func (a Alarm) find(args map[string]any) (alarms.Alarm, error) {
	all := a.M.Alarms()
	if id, _ := args["id"].(string); id != "" {
		for _, al := range all {
			if al.ID == id {
				return al, nil
			}
		}
		return alarms.Alarm{}, fmt.Errorf("нет такого будильника")
	}
	picked := all
	if clock, _ := args["time"].(string); clock != "" {
		h, m, err := parseClock(clock)
		if err != nil {
			return alarms.Alarm{}, err
		}
		picked = nil
		for _, al := range all {
			if al.Hour == h && al.Minute == m {
				picked = append(picked, al)
			}
		}
	}
	switch len(picked) {
	case 0:
		return alarms.Alarm{}, fmt.Errorf("нет такого будильника")
	case 1:
		return picked[0], nil
	}
	names := make([]string, len(picked))
	for i, al := range picked {
		names[i] = fmt.Sprintf("%d:%02d", al.Hour, al.Minute)
	}
	return alarms.Alarm{}, fmt.Errorf("будильников несколько: %s, спроси какой", strings.Join(names, ", "))
}

func parseClock(s string) (h, m int, err error) {
	hs, ms, ok := strings.Cut(strings.TrimSpace(s), ":")
	if ok {
		h, err = strconv.Atoi(hs)
		if err == nil {
			m, err = strconv.Atoi(ms)
		}
	}
	if !ok || err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("время нужно как ЧЧ:ММ, а не %q", s)
	}
	return h, m, nil
}

func parseDays(v any) ([]int, error) {
	list, _ := v.([]any)
	var out []int
	for _, d := range list {
		name, _ := d.(string)
		days, ok := isoDays[name]
		if !ok {
			return nil, fmt.Errorf("нет такого дня: %v", d)
		}
		out = append(out, days...)
	}
	return out, nil
}

// whenWords says a moment the way it is said aloud.
func whenWords(now, t time.Time) string {
	if t.IsZero() {
		return "больше не прозвенит"
	}
	clock := fmt.Sprintf("в %d:%02d", t.Hour(), t.Minute())
	y, m, d := now.Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	switch {
	case t.Before(today.AddDate(0, 0, 1)):
		return "сегодня " + clock
	case t.Before(today.AddDate(0, 0, 2)):
		return "завтра " + clock
	case t.Before(today.AddDate(0, 0, 7)):
		return onDays[t.Weekday()] + " " + clock
	}
	// "суббота, 31 октября 2026": the day and the month are what is said.
	date, _ := agent.RussianDateTime(t)
	f := strings.Fields(date)
	return f[1] + " " + f[2] + " " + clock
}

// plural picks the Russian form for n: 1 минута, 2 минуты, 5 минут.
func plural(n int, one, few, many string) string {
	form := many
	switch {
	case n%100 >= 11 && n%100 <= 14:
	case n%10 == 1:
		form = one
	case n%10 >= 2 && n%10 <= 4:
		form = few
	}
	return strconv.Itoa(n) + " " + form
}

// durationWords says a span, to the second.
func durationWords(d time.Duration) string {
	secs := int(d.Round(time.Second) / time.Second)
	var parts []string
	if h := secs / 3600; h > 0 {
		parts = append(parts, plural(h, "час", "часа", "часов"))
	}
	if m := secs % 3600 / 60; m > 0 {
		parts = append(parts, plural(m, "минута", "минуты", "минут"))
	}
	if s := secs % 60; s > 0 || len(parts) == 0 {
		parts = append(parts, plural(s, "секунда", "секунды", "секунд"))
	}
	return strings.Join(parts, " ")
}

// TimerTool sets and changes timers.
type TimerTool struct {
	M   *alarms.Manager
	Now func() time.Time // nil = time.Now
}

func (TimerTool) Decl() agent.FuncDecl {
	return agent.FuncDecl{
		Name: "timer",
		Description: "Set or change timers on this speaker; several can run at once. " +
			"pause, resume and cancel find the timer by title, or by id from alarms_list; a lone timer needs neither.",
		Params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action":  map[string]any{"type": "string", "enum": []string{"set", "pause", "resume", "cancel"}},
				"seconds": map[string]any{"type": "integer", "description": "How long, for set"},
				"title":   map[string]any{"type": "string"},
				"id":      map[string]any{"type": "string"},
			},
			"required": []string{"action"},
		},
	}
}

func (t TimerTool) Run(_ context.Context, args map[string]any) (map[string]any, error) {
	action, _ := args["action"].(string)
	switch action {
	case "set":
		secs, _ := args["seconds"].(float64)
		title, _ := args["title"].(string)
		d := time.Duration(secs) * time.Second
		tm, err := t.M.AddTimer(d, title)
		if err != nil {
			return nil, err
		}
		return map[string]any{"id": tm.ID, "left": durationWords(d)}, nil
	case "pause", "resume", "cancel":
		cur, err := t.find(args)
		if err != nil {
			return nil, err
		}
		switch action {
		case "pause":
			err = t.M.PauseTimer(cur.ID)
		case "resume":
			err = t.M.ResumeTimer(cur.ID)
		default:
			err = t.M.CancelTimer(cur.ID)
		}
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	}
	return nil, fmt.Errorf("unknown action %q", action)
}

func (t TimerTool) find(args map[string]any) (alarms.Timer, error) {
	id, _ := args["id"].(string)
	title, _ := args["title"].(string)
	var picked []alarms.Timer
	for _, tm := range t.M.Timers() {
		if (id == "" || tm.ID == id) && (title == "" || strings.EqualFold(tm.Title, title)) {
			picked = append(picked, tm)
		}
	}
	switch len(picked) {
	case 0:
		return alarms.Timer{}, fmt.Errorf("нет такого таймера")
	case 1:
		return picked[0], nil
	}
	now := orNow(t.Now)
	names := make([]string, len(picked))
	for i, tm := range picked {
		names[i] = strings.TrimSpace(tm.Title + " " + durationWords(tm.Remaining(now)))
	}
	return alarms.Timer{}, fmt.Errorf("таймеров несколько: %s, спроси какой", strings.Join(names, ", "))
}

// AlarmsList tells what is set.
type AlarmsList struct {
	M   *alarms.Manager
	Now func() time.Time // nil = time.Now
}

func (AlarmsList) Decl() agent.FuncDecl {
	return agent.FuncDecl{
		Name:        "alarms_list",
		Description: "List the alarms and timers: which are set, when they ring, how long is left.",
		Params:      map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

func (l AlarmsList) Run(context.Context, map[string]any) (map[string]any, error) {
	now := orNow(l.Now)
	as := []map[string]any{}
	for _, a := range l.M.Alarms() {
		e := map[string]any{"id": a.ID, "time": fmt.Sprintf("%d:%02d", a.Hour, a.Minute)}
		if a.Title != "" {
			e["title"] = a.Title
		}
		if next := a.Next(now); next.IsZero() {
			e["off"] = true
		} else {
			e["when"] = whenWords(now, next)
		}
		as = append(as, e)
	}
	ts := []map[string]any{}
	for _, t := range l.M.Timers() {
		e := map[string]any{"id": t.ID, "left": durationWords(t.Remaining(now))}
		if t.Title != "" {
			e["title"] = t.Title
		}
		if t.Paused {
			e["paused"] = true
		}
		ts = append(ts, e)
	}
	return map[string]any{"alarms": as, "timers": ts}, nil
}
