package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"voice/internal/alarms"
)

type quietRinger struct{}

func (quietRinger) Start(alarms.Ring) {}
func (quietRinger) Stop(bool)         {}

func alarmServer(t *testing.T) (*httptest.Server, *alarms.Manager, *Daemon) {
	t.Helper()
	d, _ := newTest(t, &fakeAct{}, "", false)
	m := alarms.New(filepath.Join(t.TempDir(), "alarms.json"), quietRinger{}, time.Now)
	d.opt.Alarms, d.opt.Ring = m, m
	return apiServer(t, d), m, d
}

func wantStatus(t *testing.T, r *http.Response, status int) {
	t.Helper()
	if r.StatusCode != status {
		t.Fatalf("%s %s: status %d, want %d", r.Request.Method, r.Request.URL.Path, r.StatusCode, status)
	}
}

func TestAlarmsAPI(t *testing.T) {
	srv, m, _ := alarmServer(t)
	url := srv.URL + apiBase
	r := call(t, "POST", url+"/alarms", `{"hour":7,"minute":30,"days":[1,2],"title":"работа","sound":"birds"}`)
	wantStatus(t, r, 200)
	var a struct {
		ID   string    `json:"id"`
		Next time.Time `json:"next"`
	}
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		t.Fatal(err)
	}
	if a.ID == "" || a.Next.IsZero() {
		t.Fatalf("%+v", a)
	}
	// What is not sent stays as it was.
	wantStatus(t, call(t, "PATCH", url+"/alarms/"+a.ID, `{"disabled":true}`), 200)
	got := m.Alarms()[0]
	if !got.Disabled || got.Hour != 7 || got.Title != "работа" || got.Sound != "birds" {
		t.Errorf("%+v", got)
	}
	wantStatus(t, call(t, "PATCH", url+"/alarms/"+a.ID, `{"disabled":false,"hour":8}`), 200)
	if got := m.Alarms()[0]; got.Disabled || got.Hour != 8 || got.ID != a.ID {
		t.Errorf("%+v", got)
	}
	r = call(t, "GET", url+"/alarms", "")
	wantStatus(t, r, 200)
	var list struct {
		Alarms []struct {
			ID string `json:"id"`
		} `json:"alarms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&list); err != nil || len(list.Alarms) != 1 {
		t.Fatalf("%+v %v", list, err)
	}
	wantStatus(t, call(t, "DELETE", url+"/alarms/"+a.ID, ""), 200)
	if len(m.Alarms()) != 0 {
		t.Error("not deleted")
	}
}

func TestAlarmsAPIRefusals(t *testing.T) {
	srv, _, _ := alarmServer(t)
	url := srv.URL + apiBase
	for _, c := range []struct {
		method, path, body string
		status             int
		code               string
	}{
		{"POST", "/alarms", `{"hour":25}`, 400, "bad_request"},
		{"POST", "/alarms", `{"hour":7,"sound":"../x"}`, 400, "bad_request"},
		{"POST", "/alarms", `{broken`, 400, "bad_request"},
		{"PATCH", "/alarms/none", `{"hour":7}`, 404, "not_found"},
		{"DELETE", "/alarms/none", "", 404, "not_found"},
		{"POST", "/timers", `{"seconds":0}`, 400, "bad_request"},
		{"PATCH", "/timers/none", `{"paused":true}`, 404, "not_found"},
		{"DELETE", "/timers/none", "", 404, "not_found"},
		{"POST", "/ring/snooze", "", 409, "conflict"},
	} {
		r := call(t, c.method, url+c.path, c.body)
		if r.StatusCode != c.status {
			t.Errorf("%s %s %s: status %d", c.method, c.path, c.body, r.StatusCode)
			continue
		}
		if code := errorCode(t, r); code != c.code {
			t.Errorf("%s %s: code %s", c.method, c.path, code)
		}
	}
}

func TestTimersAPI(t *testing.T) {
	srv, m, _ := alarmServer(t)
	url := srv.URL + apiBase
	r := call(t, "POST", url+"/timers", `{"seconds":600,"title":"чай"}`)
	wantStatus(t, r, 200)
	var tm struct {
		ID      string `json:"id"`
		Seconds int    `json:"seconds"`
		Left    int    `json:"left"`
		Title   string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&tm); err != nil {
		t.Fatal(err)
	}
	if tm.ID == "" || tm.Seconds != 600 || tm.Left != 600 || tm.Title != "чай" {
		t.Fatalf("%+v", tm)
	}
	wantStatus(t, call(t, "PATCH", url+"/timers/"+tm.ID, `{"paused":true}`), 200)
	if !m.Timers()[0].Paused {
		t.Error("not paused")
	}
	wantStatus(t, call(t, "PATCH", url+"/timers/"+tm.ID, `{"paused":false}`), 200)
	if m.Timers()[0].Paused {
		t.Error("still paused")
	}
	wantStatus(t, call(t, "DELETE", url+"/timers/"+tm.ID, ""), 200)
	if len(m.Timers()) != 0 {
		t.Error("not cancelled")
	}
}

func TestRingAPI(t *testing.T) {
	srv, m, _ := alarmServer(t)
	url := srv.URL + apiBase
	wantStatus(t, call(t, "POST", url+"/ring/test", ""), 200)
	if !m.Ringing() {
		t.Fatal("no ring")
	}
	wantStatus(t, call(t, "POST", url+"/ring/stop", ""), 200)
	if m.Ringing() {
		t.Error("still ringing")
	}
	wantStatus(t, call(t, "POST", url+"/ring/stop", ""), 200) // already over: fine
}

func TestSoundsAPI(t *testing.T) {
	srv, _, _ := alarmServer(t)
	r := call(t, "GET", srv.URL+apiBase+"/alarms/sounds", "")
	wantStatus(t, r, 200)
	var v struct {
		Sounds []alarms.SoundInfo `json:"sounds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil || len(v.Sounds) != len(alarms.Sounds) {
		t.Fatalf("%+v %v", v, err)
	}
}

func TestStateShowsAlarms(t *testing.T) {
	srv, m, d := alarmServer(t)
	if _, err := m.AddAlarm(alarms.Alarm{Hour: 7}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddTimer(time.Hour, "чай"); err != nil {
		t.Fatal(err)
	}
	m.Test()
	v := d.state()
	if v.Alarms == nil || len(v.Alarms.Alarms) != 1 || len(v.Alarms.Timers) != 1 || v.Alarms.Ring.Ring != alarms.KindAlarm {
		t.Fatalf("%+v", v.Alarms)
	}
	r := call(t, "GET", srv.URL+apiBase+"/state", "")
	var raw struct {
		Alarms map[string]json.RawMessage `json:"alarms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw.Alarms["ring"]), "alarm") {
		t.Errorf("%s", raw.Alarms["ring"])
	}
}

func TestStateWithoutAlarms(t *testing.T) {
	d, _ := newTest(t, &fakeAct{}, "", false)
	if d.state().Alarms != nil {
		t.Error("alarms from nowhere")
	}
}

// The ring's stop runs on the audio goroutine under d.mu, and it tells the panel
// of the change: that must not need d.mu.
func TestChangeNoticeUnderTheLock(t *testing.T) {
	_, m, d := alarmServer(t)
	d.WatchAlarms(m)
	m.Test()
	done := make(chan struct{})
	go func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		m.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock")
	}
}

func TestInfoNamesAlarms(t *testing.T) {
	_, _, d := alarmServer(t)
	info := d.info()["features"].(map[string]bool)
	if !info["alarms"] {
		t.Errorf("%v", info)
	}
}

func TestAPIChoosesTheMusic(t *testing.T) {
	srv, m, d := alarmServer(t)
	d.opt.FindMusic = func(_ context.Context, q, kind string) (alarms.Music, error) {
		if q == "нет" {
			return alarms.Music{}, errors.New("ничего не найдено")
		}
		return alarms.Music{Kind: kind, URI: "spotify:artist:a1", Name: "The Weeknd"}, nil
	}
	url := srv.URL + apiBase
	r := call(t, "POST", url+"/alarms", `{"hour":7,"music":"the weeknd","music_kind":"artist"}`)
	wantStatus(t, r, 200)
	a := m.Alarms()[0]
	if a.MusicItem == nil || a.MusicItem.URI != "spotify:artist:a1" {
		t.Fatalf("%+v", a)
	}
	// Something else changing leaves the choice alone; clearing the text clears it.
	wantStatus(t, call(t, "PATCH", url+"/alarms/"+a.ID, `{"hour":8}`), 200)
	if m.Alarms()[0].MusicItem == nil {
		t.Error("lost on another change")
	}
	wantStatus(t, call(t, "PATCH", url+"/alarms/"+a.ID, `{"music":""}`), 200)
	if b := m.Alarms()[0]; b.MusicItem != nil || b.Music != "" {
		t.Errorf("%+v", b)
	}
	// Not found: kept to be searched at the ring, and the panel is told.
	r = call(t, "PATCH", url+"/alarms/"+a.ID, `{"music":"нет"}`)
	wantStatus(t, r, 200)
	var v struct {
		Note string `json:"music_note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil || v.Note == "" {
		t.Errorf("%+v %v", v, err)
	}
	if b := m.Alarms()[0]; b.MusicItem != nil || b.Music != "нет" {
		t.Errorf("%+v", b)
	}
	// Liked Songs need no words.
	wantStatus(t, call(t, "PATCH", url+"/alarms/"+a.ID, `{"music":"","music_kind":"liked"}`), 200)
	if b := m.Alarms()[0]; b.MusicItem == nil || b.MusicItem.Kind != "liked" {
		t.Errorf("%+v", b)
	}
}
