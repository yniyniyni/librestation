package spotifyapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fake struct {
	t        *testing.T
	tokenReq atomic.Int32
	tokenFn  func(w http.ResponseWriter, form map[string]string)
	api      http.HandlerFunc
	c        *Client
	file     string
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{t: t}
	f.tokenFn = func(w http.ResponseWriter, _ map[string]string) {
		w.Write([]byte(`{"access_token":"AT","expires_in":3600}`))
	}
	acc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.tokenReq.Add(1)
		if r.URL.Path != "/api/token" || r.Method != http.MethodPost {
			t.Errorf("token request %s %s", r.Method, r.URL.Path)
		}
		r.ParseForm()
		form := map[string]string{}
		for k := range r.PostForm {
			form[k] = r.PostForm.Get(k)
		}
		if form["grant_type"] != "refresh_token" || form["client_id"] != "cid" {
			t.Errorf("form = %v", form)
		}
		f.tokenFn(w, form)
	}))
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer AT" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		f.api(w, r)
	}))
	t.Cleanup(acc.Close)
	t.Cleanup(apiSrv.Close)
	f.file = filepath.Join(t.TempDir(), "spotify.json")
	if err := os.WriteFile(f.file, []byte(`{"client_id":"cid","refresh_token":"RT1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f.c = New(f.file, "Станция")
	f.c.APIURL, f.c.AccountsURL = apiSrv.URL, acc.URL
	f.c.pollDelay = time.Millisecond
	f.api = func(w http.ResponseWriter, r *http.Request) { t.Errorf("unexpected %s %s", r.Method, r.URL) }
	return f
}

func TestRefreshAndCache(t *testing.T) {
	f := newFake(t)
	f.api = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }
	for i := 0; i < 2; i++ {
		if _, ok, err := f.c.Current(context.Background()); err != nil || ok {
			t.Fatalf("Current = %v, %v", ok, err)
		}
	}
	if n := f.tokenReq.Load(); n != 1 {
		t.Errorf("token requests = %d, want 1", n)
	}
}

func TestRefreshAgainNearExpiry(t *testing.T) {
	f := newFake(t)
	now := time.Now()
	f.c.now = func() time.Time { return now }
	f.api = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }
	f.c.Current(context.Background())
	now = now.Add(3541 * time.Second) // inside the 60 s slack
	f.c.Current(context.Background())
	if n := f.tokenReq.Load(); n != 2 {
		t.Errorf("token requests = %d, want 2", n)
	}
}

func TestRotatedRefreshTokenSaved(t *testing.T) {
	f := newFake(t)
	f.tokenFn = func(w http.ResponseWriter, _ map[string]string) {
		w.Write([]byte(`{"access_token":"AT","expires_in":3600,"refresh_token":"RT2"}`))
	}
	f.api = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }
	if _, _, err := f.c.Current(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f.file)
	var tf tokenFile
	if err := json.Unmarshal(b, &tf); err != nil || tf.RefreshToken != "RT2" || tf.ClientID != "cid" {
		t.Errorf("file = %s (%v)", b, err)
	}
	if st, _ := os.Stat(f.file); st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", st.Mode())
	}
}

func TestRevokedTokenIsNotLinked(t *testing.T) {
	f := newFake(t)
	f.tokenFn = func(w http.ResponseWriter, _ map[string]string) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}
	now := time.Now()
	f.c.now = func() time.Time { return now }
	if _, _, err := f.c.Current(context.Background()); !errors.Is(err, ErrNotLinked) {
		t.Fatalf("err = %v", err)
	}
	if f.c.Linked() {
		t.Error("Linked() = true after invalid_grant")
	}
	now = now.Add(30 * time.Second)
	if _, _, err := f.c.Current(context.Background()); !errors.Is(err, ErrNotLinked) {
		t.Fatalf("err = %v", err)
	}
	if n := f.tokenReq.Load(); n != 1 {
		t.Errorf("token requests = %d, want 1", n)
	}
	now = now.Add(31 * time.Second)
	f.c.Current(context.Background())
	if n := f.tokenReq.Load(); n != 2 {
		t.Errorf("token requests after 60 s = %d, want 2", n)
	}
}

func TestRevokedRetriedWhenFileChanges(t *testing.T) {
	f := newFake(t)
	fail := true
	f.tokenFn = func(w http.ResponseWriter, _ map[string]string) {
		if fail {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		w.Write([]byte(`{"access_token":"AT","expires_in":3600}`))
	}
	f.api = func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }
	f.c.Current(context.Background())
	fail = false
	future := time.Now().Add(time.Hour)
	os.WriteFile(f.file, []byte(`{"client_id":"cid","refresh_token":"RT9"}`), 0o600)
	os.Chtimes(f.file, future, future)
	if _, _, err := f.c.Current(context.Background()); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestMissingFileNotLinked(t *testing.T) {
	c := New(filepath.Join(t.TempDir(), "none.json"), "")
	if c.Linked() {
		t.Error("Linked() = true")
	}
	if _, _, err := c.Current(context.Background()); !errors.Is(err, ErrNotLinked) {
		t.Errorf("err = %v", err)
	}
}

func TestErrorsDoNotLeakTokens(t *testing.T) {
	f := newFake(t)
	f.api = func(w http.ResponseWriter, r *http.Request) { http.Error(w, "RT1 AT", 500) }
	_, _, err := f.c.Current(context.Background())
	if err == nil || strings.Contains(err.Error(), "AT") || strings.Contains(err.Error(), "RT1") {
		t.Errorf("err = %v", err)
	}
}

const searchJSON = `{
 "tracks":{"items":[{"uri":"spotify:track:t1","name":"Starboy","artists":[{"name":"The Weeknd"}]}]},
 "artists":{"items":[{"uri":"spotify:artist:a1","name":"The Weeknd"}]},
 "albums":{"items":[{"uri":"spotify:album:b1","name":"Dawn FM","artists":[{"name":"The Weeknd"}]}]},
 "playlists":{"items":[null,{"uri":"spotify:playlist:p1","name":"Weeknd mix"}]}}`

func TestSearchAnyPrefersExactArtist(t *testing.T) {
	f := newFake(t)
	var got map[string]string
	f.api = func(w http.ResponseWriter, r *http.Request) {
		got = map[string]string{"path": r.URL.Path, "type": r.URL.Query().Get("type"), "limit": r.URL.Query().Get("limit"), "q": r.URL.Query().Get("q")}
		w.Write([]byte(searchJSON))
	}
	it, err := f.c.Search(context.Background(), "the weeknd", "any")
	if err != nil {
		t.Fatal(err)
	}
	if it.URI != "spotify:artist:a1" || it.Kind != "artist" {
		t.Errorf("item = %+v", it)
	}
	if got["path"] != "/v1/search" || got["type"] != "track,artist,album,playlist" || got["limit"] != "5" {
		t.Errorf("request = %v", got)
	}
	it, err = f.c.Search(context.Background(), "starboy", "any")
	if err != nil || it.Kind != "track" || it.Artist != "The Weeknd" {
		t.Errorf("item = %+v, %v", it, err)
	}
}

func TestSearchKinds(t *testing.T) {
	f := newFake(t)
	f.api = func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(searchJSON)) }
	for kind, want := range map[string]string{"album": "spotify:album:b1", "playlist": "spotify:playlist:p1", "track": "spotify:track:t1"} {
		it, err := f.c.Search(context.Background(), "x", kind)
		if err != nil || it.URI != want {
			t.Errorf("%s: %+v, %v", kind, it, err)
		}
	}
	f.api = func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"tracks":{"items":[]}}`)) }
	if _, err := f.c.Search(context.Background(), "x", "track"); err == nil {
		t.Error("empty result: want error")
	}
}

func TestPlayTransfersWhenNotActive(t *testing.T) {
	f := newFake(t)
	var calls []string
	var playBody map[string]any
	devices := 0
	f.api = func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.URL.Path == "/v1/me/player/devices":
			devices++
			if devices == 1 {
				w.Write([]byte(`{"devices":[]}`))
				return
			}
			w.Write([]byte(`{"devices":[{"id":"D1","name":"Станция","is_active":false}]}`))
		case r.URL.Path == "/v1/me/player" && r.Method == http.MethodPut:
			var b struct {
				IDs  []string `json:"device_ids"`
				Play bool     `json:"play"`
			}
			json.NewDecoder(r.Body).Decode(&b)
			if len(b.IDs) != 1 || b.IDs[0] != "D1" || b.Play {
				t.Errorf("transfer body = %+v", b)
			}
		case r.URL.Path == "/v1/me/player/play":
			if r.URL.Query().Get("device_id") != "D1" {
				t.Errorf("device_id = %q", r.URL.Query().Get("device_id"))
			}
			json.NewDecoder(r.Body).Decode(&playBody)
		default:
			t.Errorf("unexpected %s", r.URL)
		}
	}
	err := f.c.Play(context.Background(), Item{Kind: "artist", URI: "spotify:artist:a1"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"GET /v1/me/player/devices", "GET /v1/me/player/devices", "PUT /v1/me/player", "PUT /v1/me/player/play"}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Errorf("calls = %v", calls)
	}
	if playBody["context_uri"] != "spotify:artist:a1" || playBody["uris"] != nil {
		t.Errorf("play body = %v", playBody)
	}
}

func TestPlayTrackUsesURIsAndRetriesOn404(t *testing.T) {
	f := newFake(t)
	plays, transfers := 0, 0
	var body map[string]any
	f.api = func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/me/player/devices":
			w.Write([]byte(`{"devices":[{"id":"D1","name":"Станция","is_active":true}]}`))
		case "/v1/me/player":
			transfers++
		case "/v1/me/player/play":
			plays++
			if plays == 1 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			json.NewDecoder(r.Body).Decode(&body)
		}
	}
	if err := f.c.Play(context.Background(), Item{Kind: "track", URI: "spotify:track:t1"}); err != nil {
		t.Fatal(err)
	}
	if plays != 2 || transfers != 1 {
		t.Errorf("plays=%d transfers=%d", plays, transfers)
	}
	if uris, _ := body["uris"].([]any); len(uris) != 1 || uris[0] != "spotify:track:t1" || body["context_uri"] != nil {
		t.Errorf("body = %v", body)
	}
}

func TestPlayDeviceMissing(t *testing.T) {
	f := newFake(t)
	f.api = func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/me/player/devices" {
			t.Errorf("unexpected %s", r.URL)
		}
		w.Write([]byte(`{"devices":[{"id":"X","name":"Телефон","is_active":true}]}`))
	}
	err := f.c.Play(context.Background(), Item{Kind: "artist", URI: "spotify:artist:a1"})
	if err == nil || err.Error() != "не вижу колонку в Spotify" {
		t.Errorf("err = %v", err)
	}
}

func TestPlayEmptyDeviceNamePicksActiveOrFirst(t *testing.T) {
	f := newFake(t)
	f.c.Device = ""
	var dev string
	f.api = func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/me/player/devices":
			w.Write([]byte(`{"devices":[{"id":"A","name":"a","is_active":false},{"id":"B","name":"b","is_active":true}]}`))
		case "/v1/me/player/play":
			dev = r.URL.Query().Get("device_id")
		}
	}
	if err := f.c.Play(context.Background(), Item{Kind: "album", URI: "spotify:album:b1"}); err != nil || dev != "B" {
		t.Errorf("dev=%q err=%v", dev, err)
	}
}

func TestSetLiked(t *testing.T) {
	f := newFake(t)
	var got []string
	f.api = func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path+"?uris="+r.URL.Query().Get("uris"))
	}
	f.c.SetLiked(context.Background(), "spotify:track:t1", true)
	f.c.SetLiked(context.Background(), "spotify:track:t1", false)
	if strings.Join(got, ",") != "PUT /v1/me/library?uris=spotify:track:t1,DELETE /v1/me/library?uris=spotify:track:t1" {
		t.Errorf("got = %v", got)
	}
	if err := f.c.SetLiked(context.Background(), "spotify:album:x", true); err == nil {
		t.Error("non-track: want error")
	}
}

func TestCurrent(t *testing.T) {
	f := newFake(t)
	f.api = func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"item":{"type":"track","uri":"spotify:track:t1","name":"Starboy","artists":[{"name":"The Weeknd"}]}}`))
	}
	it, ok, err := f.c.Current(context.Background())
	if err != nil || !ok || it.Name != "Starboy" || it.Artist != "The Weeknd" || it.Kind != "track" {
		t.Errorf("%+v %v %v", it, ok, err)
	}
}
