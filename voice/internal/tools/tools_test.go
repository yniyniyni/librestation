package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"voice/internal/agent"
)

const geoJSON = `{"results":[{"name":"Прага","country":"Чехия","latitude":50.08,"longitude":14.42}]}`
const forecastJSON = `{
 "current":{"temperature_2m":12.4,"apparent_temperature":10.1,"weather_code":3,"wind_speed_10m":4.5},
 "daily":{"temperature_2m_max":[14.0,17.5],"temperature_2m_min":[6.0,8.5],"precipitation_probability_max":[20,60],"weather_code":[3,61]}}`

func weatherServer(t *testing.T, geo string, gotName *string) *Weather {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/geo", func(w http.ResponseWriter, r *http.Request) {
		if gotName != nil {
			*gotName = r.URL.Query().Get("name")
		}
		if r.URL.Query().Get("language") != "ru" {
			t.Errorf("language = %q", r.URL.Query().Get("language"))
		}
		fmt.Fprint(w, geo)
	})
	mux.HandleFunc("/forecast", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, forecastJSON)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Weather{HTTP: srv.Client(), GeoURL: srv.URL + "/geo", ForecastURL: srv.URL + "/forecast",
		DefaultCity: func() string { return "Прага" }}
}

func TestWeatherPrague(t *testing.T) {
	w := weatherServer(t, geoJSON, nil)
	res, err := w.Run(context.Background(), map[string]any{"city": "Прага"})
	if err != nil {
		t.Fatal(err)
	}
	if res["city"] != "Прага" {
		t.Errorf("city = %v", res["city"])
	}
	if v, ok := res["temperature"].(float64); !ok || v != 12.4 {
		t.Errorf("temperature = %v", res["temperature"])
	}
	if d, _ := res["description"].(string); d == "" || d == "3" {
		t.Errorf("description = %q", d)
	}
	if res["max"] != 14.0 || res["min"] != 6.0 || res["precipitation_probability"] != 20 {
		t.Errorf("day = %v", res)
	}
	tm, err := w.Run(context.Background(), map[string]any{"city": "Прага", "day": "tomorrow"})
	if err != nil {
		t.Fatal(err)
	}
	if tm["max"] != 17.5 || tm["description"] != "дождь" {
		t.Errorf("tomorrow = %v", tm)
	}
	if _, ok := tm["temperature"]; ok {
		t.Errorf("tomorrow must not carry current temperature")
	}
}

func TestWeatherDefaultCity(t *testing.T) {
	var name string
	w := weatherServer(t, geoJSON, &name)
	if _, err := w.Run(context.Background(), map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if name != "Прага" {
		t.Errorf("geocoded %q", name)
	}
	w.DefaultCity = func() string { return "" }
	if _, err := w.Run(context.Background(), map[string]any{}); err == nil || !strings.Contains(err.Error(), "не знаю, какой город") {
		t.Errorf("err = %v", err)
	}
}

func TestWeatherUnknownCity(t *testing.T) {
	w := weatherServer(t, `{}`, nil)
	_, err := w.Run(context.Background(), map[string]any{"city": "Атлантида"})
	if err == nil || !strings.Contains(err.Error(), "Атлантида") {
		t.Errorf("err = %v", err)
	}
}

func TestClockRussianDate(t *testing.T) {
	c := Clock{Now: func() time.Time { return time.Date(2026, 10, 31, 21, 7, 0, 0, time.UTC) }}
	res, err := c.Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res["date"] != "суббота, 31 октября 2026" || res["time"] != "21:07" {
		t.Errorf("res = %v", res)
	}
}

type fakeAct struct{ calls []string }

func (f *fakeAct) rec(s string) error { f.calls = append(f.calls, s); return nil }
func (f *fakeAct) Pause() error       { return f.rec("pause") }
func (f *fakeAct) Play() error        { return f.rec("play") }
func (f *fakeAct) Next() error        { return f.rec("next") }
func (f *fakeAct) Prev() error        { return f.rec("prev") }
func (f *fakeAct) Step(d int) error   { return f.rec(fmt.Sprintf("step%+d", d)) }

func TestPlaybackActions(t *testing.T) {
	f := &fakeAct{}
	vol := -1
	p := Playback{A: f, SetVolume: func(v int) error { vol = v; return nil }}
	for _, a := range []string{"pause", "play", "next", "prev", "louder", "quieter"} {
		if _, err := p.Run(context.Background(), map[string]any{"action": a}); err != nil {
			t.Fatal(a, err)
		}
	}
	want := "pause play next prev step+5 step-5"
	if got := strings.Join(f.calls, " "); got != want {
		t.Errorf("calls = %q, want %q", got, want)
	}
	if _, err := p.Run(context.Background(), map[string]any{"action": "set_volume", "value": 30.0}); err != nil || vol != 30 {
		t.Errorf("vol = %d err = %v", vol, err)
	}
	for _, args := range []map[string]any{
		{"action": "set_volume"},
		{"action": "set_volume", "value": 101.0},
		{"action": "set_volume", "value": -1.0},
		{"action": "dance"},
	} {
		if _, err := p.Run(context.Background(), args); err == nil {
			t.Errorf("args %v: want error", args)
		}
	}
}

type fakeLLM struct{ req agent.Request }

func (f *fakeLLM) Stream(_ context.Context, r agent.Request, _ func(string)) (agent.Content, error) {
	f.req = r
	return agent.Content{Role: "model", Parts: []agent.Part{{Text: "Два "}, {Text: "предложения."}}}, nil
}

func TestWebAnswer(t *testing.T) {
	l := &fakeLLM{}
	w := Web{LLM: l, Model: func() string { return "m" }}
	res, err := w.Run(context.Background(), map[string]any{"question": "кто выиграл?"})
	if err != nil {
		t.Fatal(err)
	}
	if res["answer"] != "Два предложения." {
		t.Errorf("answer = %v", res["answer"])
	}
	if !l.req.GoogleSearch || len(l.req.Funcs) != 0 || l.req.Model != "m" {
		t.Errorf("req = %+v", l.req)
	}
	if _, err := w.Run(context.Background(), map[string]any{}); err == nil {
		t.Error("empty question: want error")
	}
}
