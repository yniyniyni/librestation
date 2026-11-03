package daemon

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"voice/internal/kws"
)

type fakeVolume struct {
	vol, knob int
	steps     []int
}

func (f *fakeVolume) Volume() int   { return f.vol }
func (f *fakeVolume) KnobStep() int { return f.knob }
func (f *fakeVolume) SetKnobStep(n int) error {
	f.knob = n
	return nil
}
func (f *fakeVolume) Step(d int) error {
	f.steps = append(f.steps, d)
	f.vol += d
	return nil
}

func apiServer(t *testing.T, d *Daemon) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func errorCode(t *testing.T, r *http.Response) string {
	t.Helper()
	var e struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		t.Fatal(err)
	}
	if e.Error.Message == "" {
		t.Fatalf("no error message, status %d", r.StatusCode)
	}
	return e.Error.Code
}

func TestErrorsHaveOneShape(t *testing.T) {
	d, _ := newTest(t, &fakeAct{}, "", false)
	srv := apiServer(t, d)
	for _, c := range []struct {
		method, path, body string
		status             int
		code               string
	}{
		{"GET", "/api/v1/nothing", "", 404, "not_found"},
		{"GET", "/api/state", "", 404, "not_found"}, // the old paths are gone
		{"PATCH", "/api/v1/words/nope", `{}`, 404, "not_found"},
		{"PATCH", "/api/v1/words/x", `{bad`, 400, "bad_request"},
		{"PUT", "/api/v1/hits/9/verdict", `{"verdict":"ok"}`, 404, "not_found"},
		{"GET", "/api/v1/hits/9/audio", "", 404, "not_found"},
		{"POST", "/api/v1/enroll/train", ``, 409, "conflict"},
		{"POST", "/api/v1/player/dance", ``, 404, "not_found"},
		{"PATCH", "/api/v1/volume", `{"step":1}`, 409, "conflict"},
	} {
		r := call(t, c.method, srv.URL+c.path, c.body)
		if r.StatusCode != c.status {
			t.Errorf("%s %s: status %d, want %d", c.method, c.path, r.StatusCode, c.status)
			continue
		}
		if got := errorCode(t, r); got != c.code {
			t.Errorf("%s %s: code %q, want %q", c.method, c.path, got, c.code)
		}
	}
}

func TestOtherSitesCannotChangeAnything(t *testing.T) {
	d, _ := newTest(t, &fakeAct{}, "", false)
	srv := apiServer(t, d)
	do := func(origin string) *http.Response {
		req, _ := http.NewRequest("PATCH", srv.URL+"/api/v1/settings", strings.NewReader(`{"auto_false_undo":true}`))
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { r.Body.Close() })
		return r
	}
	r := do("http://evil.example")
	if r.StatusCode != 403 || errorCode(t, r) != "forbidden" {
		t.Fatalf("status %d", r.StatusCode)
	}
	if d.settings.AutoFalseUndo {
		t.Fatal("the change went through")
	}
	if r := do(srv.URL); r.StatusCode != 200 { // the panel itself
		t.Fatalf("same origin: status %d", r.StatusCode)
	}
	if r := do(""); r.StatusCode != 200 { // an app
		t.Fatalf("no origin: status %d", r.StatusCode)
	}
	if r, err := http.Get(srv.URL + "/api/v1/info"); err != nil || r.StatusCode != 200 {
		t.Fatalf("info: %v %v", r, err)
	} else {
		r.Body.Close()
	}
}

func TestVolumeAndPlayer(t *testing.T) {
	v := &fakeVolume{vol: 40, knob: 2}
	f := &fakeAct{}
	d, err := New(Options{DataDir: t.TempDir(), Volume: v, Actuator: f})
	if err != nil {
		t.Fatal(err)
	}
	srv := apiServer(t, d)
	r := patch(t, srv.URL+"/api/v1/volume", `{"step":5,"knob_step":3}`)
	var got volumeView
	if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Volume != 45 || got.KnobStep != 3 {
		t.Fatalf("%+v", got)
	}
	if s := d.state(); s.Volume == nil || s.Volume.Volume != 45 {
		t.Fatalf("state volume %+v", s.Volume)
	}
	if r := patch(t, srv.URL+"/api/v1/volume", `{"step":500}`); r.StatusCode != 400 {
		t.Fatalf("step 500: status %d", r.StatusCode)
	}
	if r := post(t, srv.URL+"/api/v1/player/pause", ""); r.StatusCode != 200 {
		t.Fatalf("pause: status %d", r.StatusCode)
	}
	if got := f.got(); len(got) != 1 || got[0] != "pause" {
		t.Fatalf("actuator calls %v", got)
	}
}

// The state is read under the audio goroutine's lock, so it must come from
// memory: a clip added behind the daemon's back stays unseen until a refresh.
func TestStateDoesNotReadDisk(t *testing.T) {
	d, _ := newTest(t, &fakeAct{}, "", false)
	d.words = []word{{ID: "x", Model: &kws.Model{Name: "x"}}}
	dir := d.wordDir("x")
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, "false-1.wav"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if n := d.state().Words[0].FalseClips; n != 0 {
		t.Fatalf("state saw the disk: %d clips", n)
	}
	d.refreshFalses("x")
	if n := d.state().Words[0].FalseClips; n != 1 {
		t.Fatalf("after refresh: %d clips", n)
	}
}

func TestStateFollowsFalseClips(t *testing.T) {
	d, _ := newTest(t, &fakeAct{}, "", false)
	d.words = []word{{ID: "x", Model: &kws.Model{Name: "x"}}}
	srv := apiServer(t, d)
	hit(d)
	d.waitIO()
	if r := put(t, srv.URL+"/api/v1/hits/1/verdict", `{"verdict":"false"}`); r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	if n := d.state().Words[0].FalseClips; n != 1 {
		t.Fatalf("after false: %d clips", n)
	}
	if r := put(t, srv.URL+"/api/v1/hits/1/verdict", `{"verdict":"ok"}`); r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	if n := d.state().Words[0].FalseClips; n != 0 {
		t.Fatalf("after ok: %d clips", n)
	}
}

func TestPanelIsCompressedAndCached(t *testing.T) {
	d, _ := newTest(t, &fakeAct{}, "", false)
	srv := apiServer(t, d)
	tr := &http.Transport{DisableCompression: true}
	c := &http.Client{Transport: tr}
	req, _ := http.NewRequest("GET", srv.URL+"/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	r, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("not compressed")
	}
	z, err := gzip.NewReader(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(z)
	if string(b) != string(indexHTML) {
		t.Fatal("wrong body")
	}
	req, _ = http.NewRequest("GET", srv.URL+"/", nil)
	req.Header.Set("If-None-Match", r.Header.Get("ETag"))
	r2, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusNotModified {
		t.Fatalf("status %d, want 304", r2.StatusCode)
	}
}

// sse reads events from an open stream.
func sse(t *testing.T, url string) (<-chan [2]string, func()) {
	t.Helper()
	r, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	ch := make(chan [2]string, 64)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(r.Body)
		sc.Buffer(nil, 1<<20)
		var event string
		for sc.Scan() {
			l := sc.Text()
			if after, ok := strings.CutPrefix(l, "event: "); ok {
				event = after
			} else if after, ok := strings.CutPrefix(l, "data: "); ok {
				ch <- [2]string{event, after}
			}
		}
	}()
	return ch, func() { r.Body.Close() }
}

func next(t *testing.T, ch <-chan [2]string, event string) string {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				t.Fatalf("stream ended waiting for %q", event)
			}
			if e[0] == event {
				return e[1]
			}
		case <-deadline:
			t.Fatalf("no %q event", event)
		}
	}
}

func TestEvents(t *testing.T) {
	d, _ := newTest(t, &fakeAct{}, "", false)
	srv := apiServer(t, d)
	ch, closeStream := sse(t, srv.URL+"/api/v1/events?live=1")
	defer closeStream()
	next(t, ch, "state")
	next(t, ch, "hits")
	if data := next(t, ch, "live"); !strings.Contains(data, `"scores"`) {
		t.Fatalf("live: %s", data)
	}
	// A change through the API reaches the stream at once, without a tick.
	if r := patch(t, srv.URL+"/api/v1/settings", `{"auto_false_undo":true}`); r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	if data := next(t, ch, "state"); !strings.Contains(data, `"auto_false_undo":true`) {
		t.Fatalf("state: %s", data)
	}
	hit(d)
	if data := next(t, ch, "hits"); !strings.Contains(data, `"word":"x"`) {
		t.Fatalf("hits: %s", data)
	}
}

func TestEventsWithoutLiveStayQuiet(t *testing.T) {
	d, _ := newTest(t, &fakeAct{}, "", false)
	srv := apiServer(t, d)
	ch, closeStream := sse(t, srv.URL+"/api/v1/events")
	defer closeStream()
	next(t, ch, "state")
	next(t, ch, "hits")
	select {
	case e := <-ch:
		t.Fatalf("unexpected event while nothing changed: %s %s", e[0], e[1])
	case <-time.After(3 * liveEvery * stateTicks / 2):
	}
}

func TestEventsLimitAndHubStops(t *testing.T) {
	d, _ := newTest(t, &fakeAct{}, "", false)
	srv := apiServer(t, d)
	var closers []func()
	for range maxSubscribers {
		_, c := sse(t, srv.URL+"/api/v1/events")
		closers = append(closers, c)
	}
	r, err := http.Get(srv.URL + "/api/v1/events")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 503 || errorCode(t, r) != "too_many_subscribers" {
		t.Fatalf("status %d", r.StatusCode)
	}
	for _, c := range closers {
		c()
	}
	// With no subscribers the hub is gone.
	deadline := time.Now().Add(3 * time.Second)
	for {
		d.hub.mu.Lock()
		stopped := d.hub.stop == nil && len(d.hub.subs) == 0
		d.hub.mu.Unlock()
		if stopped {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("hub still running")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAssistantViewNamesTheLiveModel(t *testing.T) {
	d, err := New(Options{DataDir: t.TempDir(), LiveModel: "gemini-3.8-live"})
	if err != nil {
		t.Fatal(err)
	}
	if v := d.assistantViewLocked(); v.LiveModel != "gemini-3.8-live" {
		t.Errorf("live model %q", v.LiveModel)
	}
}
