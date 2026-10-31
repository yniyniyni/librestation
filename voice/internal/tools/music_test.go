package tools

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"voice/internal/spotifyapi"
)

func musicClient(t *testing.T, api http.HandlerFunc) *spotifyapi.Client {
	t.Helper()
	acc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"access_token":"AT","expires_in":3600}`))
	}))
	srv := httptest.NewServer(api)
	t.Cleanup(acc.Close)
	t.Cleanup(srv.Close)
	file := filepath.Join(t.TempDir(), "spotify.json")
	os.WriteFile(file, []byte(`{"client_id":"cid","refresh_token":"RT"}`), 0o600)
	c := spotifyapi.New(file, "Станция")
	c.APIURL, c.AccountsURL = srv.URL, acc.URL
	return c
}

const currentJSON = `{"item":{"type":"track","uri":"spotify:track:t1","name":"Starboy","artists":[{"name":"The Weeknd"}]}}`

func TestLikeCurrentTrack(t *testing.T) {
	var method, uris string
	c := musicClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/me/player/currently-playing" {
			w.Write([]byte(currentJSON))
			return
		}
		method, uris = r.Method, r.URL.Query().Get("uris")
	})
	res, err := LikeTrack{S: c}.Run(context.Background(), map[string]any{"liked": true})
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPut || uris != "spotify:track:t1" {
		t.Errorf("library call = %s uris=%q", method, uris)
	}
	if res["track"] != "Starboy" || res["liked"] != true {
		t.Errorf("res = %v", res)
	}
	if _, err := (LikeTrack{S: c}).Run(context.Background(), map[string]any{"liked": false}); err != nil || method != http.MethodDelete {
		t.Errorf("unlike: %s %v", method, err)
	}
}

func TestNowPlaying(t *testing.T) {
	c := musicClient(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(currentJSON)) })
	res, err := NowPlaying{S: c}.Run(context.Background(), nil)
	if err != nil || res["track"] != "Starboy" || res["artist"] != "The Weeknd" {
		t.Errorf("res=%v err=%v", res, err)
	}
	idle := musicClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	if _, err := (NowPlaying{S: idle}).Run(context.Background(), nil); err == nil {
		t.Error("nothing playing: want error")
	}
}

func TestPlayMusic(t *testing.T) {
	var playPath string
	c := musicClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/search":
			w.Write([]byte(`{"artists":{"items":[{"uri":"spotify:artist:a1","name":"The Weeknd"}]},"tracks":{"items":[]}}`))
		case "/v1/me/player/devices":
			w.Write([]byte(`{"devices":[{"id":"D1","name":"Станция","is_active":true}]}`))
		case "/v1/me/player/play":
			playPath = r.URL.Path
		}
	})
	res, err := PlayMusic{S: c}.Run(context.Background(), map[string]any{"query": "the weeknd"})
	if err != nil || res["playing"] != "The Weeknd" || playPath == "" {
		t.Errorf("res=%v err=%v", res, err)
	}
	if _, err := (PlayMusic{S: c}).Run(context.Background(), map[string]any{}); err == nil {
		t.Error("empty query: want error")
	}
}

func TestMusicNotLinked(t *testing.T) {
	c := spotifyapi.New(filepath.Join(t.TempDir(), "none.json"), "")
	for _, run := range []func() error{
		func() error {
			_, err := PlayMusic{S: c}.Run(context.Background(), map[string]any{"query": "x"})
			return err
		},
		func() error { _, err := LikeTrack{S: c}.Run(context.Background(), nil); return err },
		func() error { _, err := NowPlaying{S: c}.Run(context.Background(), nil); return err },
	} {
		if err := run(); !errors.Is(err, spotifyapi.ErrNotLinked) {
			t.Errorf("err = %v", err)
		}
	}
}
