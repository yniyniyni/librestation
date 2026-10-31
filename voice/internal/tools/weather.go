package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"voice/internal/agent"
)

const (
	defaultGeoURL      = "https://geocoding-api.open-meteo.com/v1/search"
	defaultForecastURL = "https://api.open-meteo.com/v1/forecast"
)

// Weather reports the weather from open-meteo.
type Weather struct {
	HTTP        *http.Client // nil = a client with a 10s timeout
	DefaultCity func() string
	GeoURL      string // "" = open-meteo
	ForecastURL string // "" = open-meteo
}

func (Weather) Decl() agent.FuncDecl {
	return agent.FuncDecl{
		Name:        "get_weather",
		Description: "Get the weather for today or tomorrow in a city (the default city if none is given).",
		Params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"city": map[string]any{"type": "string", "description": "City name; omit for the default city"},
				"day":  map[string]any{"type": "string", "enum": []string{"today", "tomorrow"}},
			},
		},
	}
}

func (w Weather) client() *http.Client {
	if w.HTTP != nil {
		return w.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (w Weather) getJSON(ctx context.Context, base string, q url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := w.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("погода: %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (w Weather) Run(ctx context.Context, args map[string]any) (map[string]any, error) {
	city, _ := args["city"].(string)
	city = strings.TrimSpace(city)
	if city == "" && w.DefaultCity != nil {
		city = strings.TrimSpace(w.DefaultCity())
	}
	if city == "" {
		return nil, fmt.Errorf("не знаю, какой город")
	}
	day := 0
	if d, _ := args["day"].(string); d == "tomorrow" {
		day = 1
	}

	geoURL, fcURL := w.GeoURL, w.ForecastURL
	if geoURL == "" {
		geoURL = defaultGeoURL
	}
	if fcURL == "" {
		fcURL = defaultForecastURL
	}

	var geo struct {
		Results []struct {
			Name      string  `json:"name"`
			Country   string  `json:"country"`
			Latitude  float64 `json:"latitude"`
			Longitude float64 `json:"longitude"`
		} `json:"results"`
	}
	err := w.getJSON(ctx, geoURL, url.Values{"name": {city}, "count": {"1"}, "language": {"ru"}}, &geo)
	if err != nil {
		return nil, err
	}
	if len(geo.Results) == 0 {
		return nil, fmt.Errorf("город %q не найден", city)
	}
	g := geo.Results[0]

	var fc struct {
		Current struct {
			Temp   float64 `json:"temperature_2m"`
			Feels  float64 `json:"apparent_temperature"`
			Code   int     `json:"weather_code"`
			WindMS float64 `json:"wind_speed_10m"`
		} `json:"current"`
		Daily struct {
			Max  []float64 `json:"temperature_2m_max"`
			Min  []float64 `json:"temperature_2m_min"`
			Prec []float64 `json:"precipitation_probability_max"`
			Code []int     `json:"weather_code"`
		} `json:"daily"`
	}
	err = w.getJSON(ctx, fcURL, url.Values{
		"latitude":        {fmt.Sprint(g.Latitude)},
		"longitude":       {fmt.Sprint(g.Longitude)},
		"current":         {"temperature_2m,apparent_temperature,weather_code,wind_speed_10m"},
		"daily":           {"temperature_2m_max,temperature_2m_min,precipitation_probability_max,weather_code"},
		"wind_speed_unit": {"ms"},
		"timezone":        {"auto"},
		"forecast_days":   {"2"},
	}, &fc)
	if err != nil {
		return nil, err
	}
	d := fc.Daily
	if len(d.Max) <= day || len(d.Min) <= day || len(d.Code) <= day {
		return nil, fmt.Errorf("в прогнозе нет данных на этот день")
	}

	res := map[string]any{
		"city":    g.Name,
		"country": g.Country,
		"min":     d.Min[day],
		"max":     d.Max[day],
	}
	if len(d.Prec) > day {
		res["precipitation_probability"] = int(d.Prec[day])
	}
	if day == 0 {
		res["temperature"] = fc.Current.Temp
		res["feels_like"] = fc.Current.Feels
		res["wind_ms"] = fc.Current.WindMS
		res["description"] = weatherDesc(fc.Current.Code)
	} else {
		res["description"] = weatherDesc(d.Code[day])
	}
	return res, nil
}

// weatherDesc describes a WMO weather code in Russian.
func weatherDesc(code int) string {
	switch {
	case code == 0:
		return "ясно"
	case code == 1:
		return "преимущественно ясно"
	case code == 2:
		return "переменная облачность"
	case code == 3:
		return "облачно"
	case code == 45 || code == 48:
		return "туман"
	case code >= 51 && code <= 57:
		return "морось"
	case code >= 61 && code <= 65, code >= 80 && code <= 82:
		return "дождь"
	case code == 66 || code == 67:
		return "ледяной дождь"
	case code >= 71 && code <= 77, code == 85 || code == 86:
		return "снег"
	case code >= 95:
		return "гроза"
	}
	return "без особенностей"
}
