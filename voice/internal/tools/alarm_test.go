package tools

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"voice/internal/alarms"
)

type noRing struct{}

func (noRing) Start(alarms.Ring) {}
func (noRing) Stop(bool)         {}

type alarmRig struct {
	t   *testing.T
	m   *alarms.Manager
	now time.Time
	a   Alarm
	tm  TimerTool
	l   AlarmsList
}

func newAlarmRig(t *testing.T) *alarmRig {
	g := &alarmRig{t: t, now: time.Date(2026, 11, 6, 20, 0, 0, 0, time.Local)} // a Friday evening
	clock := func() time.Time { return g.now }
	g.m = alarms.New(filepath.Join(t.TempDir(), "alarms.json"), noRing{}, clock)
	g.a, g.tm, g.l = Alarm{M: g.m, Now: clock}, TimerTool{M: g.m, Now: clock}, AlarmsList{M: g.m, Now: clock}
	return g
}

func (g *alarmRig) alarm(args map[string]any) map[string]any {
	g.t.Helper()
	r, err := g.a.Run(context.Background(), args)
	if err != nil {
		g.t.Fatal(err)
	}
	return r
}

func (g *alarmRig) timer(args map[string]any) map[string]any {
	g.t.Helper()
	r, err := g.tm.Run(context.Background(), args)
	if err != nil {
		g.t.Fatal(err)
	}
	return r
}

func TestSetAlarmSaysWhen(t *testing.T) {
	cases := []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"time": "07:00"}, "завтра в 7:00"},
		{map[string]any{"time": "21:30"}, "сегодня в 21:30"},
		{map[string]any{"time": "07:30", "days": []any{"weekdays"}}, "в понедельник в 7:30"},
		{map[string]any{"time": "09:00", "days": []any{"weekends"}}, "завтра в 9:00"},
		{map[string]any{"time": "08:00", "days": []any{"tue"}}, "во вторник в 8:00"},
		{map[string]any{"time": "08:00", "days": []any{"wed"}}, "в среду в 8:00"},
		{map[string]any{"time": "08:00", "days": []any{"sun"}}, "в воскресенье в 8:00"},
		{map[string]any{"time": "08:00", "date": "2026-12-31"}, "31 декабря в 8:00"},
	}
	for _, c := range cases {
		g := newAlarmRig(t)
		args := c.args
		args["action"] = "set"
		if got := g.alarm(args)["when"]; got != c.want {
			t.Errorf("%v: %v, want %s", c.args, got, c.want)
		}
	}
}

func TestSetAlarmKeepsWhatWasAsked(t *testing.T) {
	g := newAlarmRig(t)
	g.alarm(map[string]any{"action": "set", "time": "7:05", "days": []any{"mon", "fri"}, "title": "работа",
		"sound": "birds", "music": "The Weeknd"})
	a := g.m.Alarms()[0]
	if a.Hour != 7 || a.Minute != 5 || len(a.Days) != 2 || a.Days[1] != 5 || a.Title != "работа" || a.Sound != "birds" || a.Music != "The Weeknd" {
		t.Errorf("%+v", a)
	}
}

func TestAlarmBadInput(t *testing.T) {
	bad := []map[string]any{
		{"action": "set"},
		{"action": "set", "time": "25:00"},
		{"action": "set", "time": "завтра"},
		{"action": "set", "time": "7:00", "days": []any{"someday"}},
		{"action": "set", "time": "7:00", "date": "вчера"},
		{"action": "set", "time": "7:00", "sound": "../x"},
		{"action": "fly"},
	}
	for _, args := range bad {
		g := newAlarmRig(t)
		if _, err := g.a.Run(context.Background(), args); err == nil {
			t.Errorf("%v passed", args)
		}
		if len(g.m.Alarms()) != 0 {
			t.Errorf("%v set something", args)
		}
	}
}

func TestAlarmFoundByTime(t *testing.T) {
	g := newAlarmRig(t)
	g.alarm(map[string]any{"action": "set", "time": "07:00"})
	g.alarm(map[string]any{"action": "set", "time": "08:00"})
	g.alarm(map[string]any{"action": "disable", "time": "07:00"})
	as := g.m.Alarms()
	if !as[0].Disabled || as[1].Disabled {
		t.Errorf("%+v", as)
	}
	g.alarm(map[string]any{"action": "enable", "time": "7:00"})
	if g.m.Alarms()[0].Disabled {
		t.Error("still off")
	}
	g.alarm(map[string]any{"action": "delete", "time": "08:00"})
	if len(g.m.Alarms()) != 1 {
		t.Errorf("%+v", g.m.Alarms())
	}
}

func TestLoneAlarmNeedsNoName(t *testing.T) {
	g := newAlarmRig(t)
	g.alarm(map[string]any{"action": "set", "time": "07:00"})
	g.alarm(map[string]any{"action": "delete"})
	if len(g.m.Alarms()) != 0 {
		t.Errorf("%+v", g.m.Alarms())
	}
}

// With several, the error names them so that the model can ask.
func TestAmbiguousAlarmIsRefused(t *testing.T) {
	g := newAlarmRig(t)
	g.alarm(map[string]any{"action": "set", "time": "07:00"})
	g.alarm(map[string]any{"action": "set", "time": "08:00"})
	_, err := g.a.Run(context.Background(), map[string]any{"action": "delete"})
	if err == nil || !strings.Contains(err.Error(), "7:00") || !strings.Contains(err.Error(), "8:00") {
		t.Fatalf("err = %v", err)
	}
	if len(g.m.Alarms()) != 2 {
		t.Error("deleted a guess")
	}
}

func TestSnoozeWithNothingToSnooze(t *testing.T) {
	g := newAlarmRig(t)
	if _, err := g.a.Run(context.Background(), map[string]any{"action": "snooze"}); err == nil {
		t.Error("snoozed nothing")
	}
}

func TestTimerSetAndLeft(t *testing.T) {
	g := newAlarmRig(t)
	if got := g.timer(map[string]any{"action": "set", "seconds": float64(600), "title": "чай"})["left"]; got != "10 минут" {
		t.Errorf("left %v", got)
	}
	g.now = g.now.Add(48 * time.Second)
	r, err := g.l.Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := r["timers"].([]map[string]any)
	if len(ts) != 1 || ts[0]["left"] != "9 минут 12 секунд" || ts[0]["title"] != "чай" {
		t.Errorf("%+v", ts)
	}
}

func TestTimerBadInput(t *testing.T) {
	g := newAlarmRig(t)
	for _, args := range []map[string]any{{"action": "set"}, {"action": "set", "seconds": float64(0)},
		{"action": "set", "seconds": float64(-5)}, {"action": "pause"}, {"action": "fly"}} {
		if _, err := g.tm.Run(context.Background(), args); err == nil {
			t.Errorf("%v passed", args)
		}
	}
}

func TestTimerActionsFindTheTimer(t *testing.T) {
	g := newAlarmRig(t)
	g.timer(map[string]any{"action": "set", "seconds": float64(60), "title": "чай"})
	g.timer(map[string]any{"action": "pause"})
	if !g.m.Timers()[0].Paused {
		t.Error("not paused")
	}
	g.timer(map[string]any{"action": "resume"})
	g.timer(map[string]any{"action": "set", "seconds": float64(120), "title": "суп"})
	if _, err := g.tm.Run(context.Background(), map[string]any{"action": "cancel"}); err == nil ||
		!strings.Contains(err.Error(), "чай") || !strings.Contains(err.Error(), "суп") {
		t.Fatalf("err = %v", err)
	}
	g.timer(map[string]any{"action": "cancel", "title": "чай"})
	if ts := g.m.Timers(); len(ts) != 1 || ts[0].Title != "суп" {
		t.Errorf("%+v", ts)
	}
}

func TestListEmpty(t *testing.T) {
	g := newAlarmRig(t)
	r, err := g.l.Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r["alarms"].([]map[string]any)) != 0 || len(r["timers"].([]map[string]any)) != 0 {
		t.Errorf("%+v", r)
	}
}

func TestDurationWords(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{time.Second, "1 секунда"}, {2 * time.Second, "2 секунды"}, {5 * time.Second, "5 секунд"}, {21 * time.Second, "21 секунда"},
		{11 * time.Second, "11 секунд"}, {time.Minute, "1 минута"}, {90 * time.Second, "1 минута 30 секунд"},
		{time.Hour, "1 час"}, {2*time.Hour + 5*time.Minute, "2 часа 5 минут"}, {0, "0 секунд"},
		{1500 * time.Millisecond, "2 секунды"},
	} {
		if got := durationWords(c.d); got != c.want {
			t.Errorf("%v: %q, want %q", c.d, got, c.want)
		}
	}
}

func TestFindMusicByKind(t *testing.T) {
	var q, typ string
	c := musicClient(t, func(w http.ResponseWriter, r *http.Request) {
		q, typ = r.URL.Query().Get("q"), r.URL.Query().Get("type")
		w.Write([]byte(`{"artists":{"items":[{"uri":"spotify:artist:a1","name":"The Weeknd"}]}}`))
	})
	m, err := FindMusic(context.Background(), c, "the weeknd", "artist")
	if err != nil {
		t.Fatal(err)
	}
	if q != "the weeknd" || typ != "artist" || m.Kind != "artist" || m.URI != "spotify:artist:a1" || m.Name != "The Weeknd" {
		t.Errorf("q %q type %q: %+v", q, typ, m)
	}
}

func TestFindMusicLiked(t *testing.T) {
	c := musicClient(t, func(w http.ResponseWriter, r *http.Request) { t.Errorf("no call needed: %s", r.URL) })
	m, err := FindMusic(context.Background(), c, "", "liked")
	if err != nil || m.Kind != "liked" || m.URI == "" {
		t.Errorf("%+v, %v", m, err)
	}
}

func TestFindMusicByLink(t *testing.T) {
	var path string
	c := musicClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Write([]byte(`{"uri":"spotify:track:0VjI","name":"Blinding Lights","artists":[{"name":"The Weeknd"}],"album":{"uri":"spotify:album:al1"}}`))
	})
	for _, link := range []string{
		"https://open.spotify.com/track/0VjI?si=abc",
		"https://open.spotify.com/intl-ru/track/0VjI",
		"spotify:track:0VjI",
	} {
		m, err := FindMusic(context.Background(), c, link, "any")
		if err != nil {
			t.Fatalf("%s: %v", link, err)
		}
		if path != "/v1/tracks/0VjI" || m.URI != "spotify:track:0VjI" || m.Name != "Blinding Lights" || m.Artist != "The Weeknd" || m.Album != "spotify:album:al1" {
			t.Errorf("%s: path %s: %+v", link, path, m)
		}
	}
}

// Whatever Spotify says of a link, the track asked for is what plays.
func TestFindMusicLinkWithoutDetails(t *testing.T) {
	c := musicClient(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusNotFound) })
	if _, err := FindMusic(context.Background(), c, "https://open.spotify.com/track/gone", "any"); err == nil {
		t.Error("a link to nothing passed")
	}
}

func TestSetAlarmChoosesTheMusicNow(t *testing.T) {
	g := newAlarmRig(t)
	var gotQ, gotKind string
	g.a.Find = func(_ context.Context, q, kind string) (alarms.Music, error) {
		gotQ, gotKind = q, kind
		return alarms.Music{Kind: "track", URI: "spotify:track:t1", Name: "Blinding Lights", Artist: "The Weeknd"}, nil
	}
	r := g.alarm(map[string]any{"action": "set", "time": "07:00", "music": "блайндинг лайтс", "music_kind": "track"})
	if gotQ != "блайндинг лайтс" || gotKind != "track" {
		t.Errorf("searched %q %q", gotQ, gotKind)
	}
	if r["music"] != "Blinding Lights, The Weeknd" {
		t.Errorf("%v", r)
	}
	if a := g.m.Alarms()[0]; a.MusicItem == nil || a.MusicItem.URI != "spotify:track:t1" {
		t.Errorf("%+v", a)
	}
}

// Liked Songs take no query.
func TestSetAlarmOnLiked(t *testing.T) {
	g := newAlarmRig(t)
	g.a.Find = func(_ context.Context, q, kind string) (alarms.Music, error) {
		return alarms.Music{Kind: kind, URI: "spotify:collection", Name: "Любимые треки"}, nil
	}
	g.alarm(map[string]any{"action": "set", "time": "07:00", "music_kind": "liked"})
	if a := g.m.Alarms()[0]; a.MusicItem == nil || a.MusicItem.Kind != "liked" {
		t.Errorf("%+v", a)
	}
}

// No network now is no reason to lose the alarm: it searches when it rings.
func TestSetAlarmWhenTheMusicCannotBeFound(t *testing.T) {
	g := newAlarmRig(t)
	g.a.Find = func(context.Context, string, string) (alarms.Music, error) {
		return alarms.Music{}, errors.New("no network")
	}
	r := g.alarm(map[string]any{"action": "set", "time": "07:00", "music": "The Weeknd"})
	if r["music_note"] == nil {
		t.Errorf("%v", r)
	}
	if a := g.m.Alarms()[0]; a.MusicItem != nil || a.Music != "The Weeknd" {
		t.Errorf("%+v", a)
	}
}
