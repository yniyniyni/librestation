package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeAct struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeAct) rec(s string) error {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
	return nil
}
func (f *fakeAct) Pause() error { return f.rec("pause") }
func (f *fakeAct) Play() error  { return f.rec("play") }
func (f *fakeAct) Next() error  { return f.rec("next") }
func (f *fakeAct) Prev() error  { return f.rec("prev") }
func (f *fakeAct) Step(d int) error {
	if d > 0 {
		return f.rec("louder")
	}
	return f.rec("quieter")
}
func (f *fakeAct) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func newTest(t *testing.T, act Actuator, action string, dry bool) (*Daemon, chan struct{}) {
	t.Helper()
	d, err := New(Options{DataDir: t.TempDir(), Actuator: act})
	if err != nil {
		t.Fatal(err)
	}
	d.settings.Words["x"] = &WordSettings{Enabled: true, Action: action}
	d.settings.DryRun = dry
	done := make(chan struct{}, 4)
	d.onActionDone = func() { done <- struct{}{} }
	return d, done
}

func hit(d *Daemon) {
	d.mu.Lock()
	d.onHit("x", 0.9)
	d.mu.Unlock()
}

func TestHitRunsAction(t *testing.T) {
	for action, want := range map[string]string{"next": "next", "prev": "prev", "pause": "pause", "play": "play", "louder": "louder", "quieter": "quieter"} {
		f := &fakeAct{}
		d, done := newTest(t, f, action, false)
		hit(d)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("action not run")
		}
		if g := f.got(); len(g) != 1 || g[0] != want {
			t.Fatalf("%s: calls %v", action, g)
		}
		if !d.state().Hits[0].Fired {
			t.Fatal("hit not marked fired")
		}
	}
}

func TestNoActionDoesNothing(t *testing.T) {
	f := &fakeAct{}
	d, _ := newTest(t, f, "", false)
	hit(d)
	time.Sleep(50 * time.Millisecond)
	if len(f.got()) != 0 || len(d.state().Hits) != 1 || d.state().Hits[0].Fired {
		t.Fatal("word without action must only be logged")
	}
}

func TestDryRunDoesNotAct(t *testing.T) {
	f := &fakeAct{}
	d, _ := newTest(t, f, "next", true)
	hit(d)
	time.Sleep(50 * time.Millisecond)
	if len(f.got()) != 0 || len(d.state().Hits) != 1 {
		t.Fatalf("dry run: calls %v", f.got())
	}
}

func TestHitKeepsAudio(t *testing.T) {
	d, _ := newTest(t, &fakeAct{}, "next", true)
	blk := make([]int16, 320)
	for i := range blk {
		blk[i] = int16(i + 1)
	}
	for i := 0; i < 5*50; i++ { // 5 s of 20 ms blocks
		d.onAudio(blk)
	}
	hit(d)
	srv := httptest.NewServer(d.Handler())
	defer srv.Close()
	var st struct{ Hits []struct{ ID int } }
	r, _ := http.Get(srv.URL + "/api/state")
	json.NewDecoder(r.Body).Decode(&st)
	r.Body.Close()
	if len(st.Hits) != 1 {
		t.Fatalf("hits %v", st.Hits)
	}
	w, err := http.Get(srv.URL + "/api/hits/1")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Body.Close()
	var n int
	buf := make([]byte, 4096)
	for {
		k, err := w.Body.Read(buf)
		n += k
		if err != nil {
			break
		}
	}
	if want := 44 + 3*16000*2; n != want {
		t.Fatalf("wav %d bytes, want %d", n, want)
	}
}

func post(t *testing.T, url, body string) *http.Response {
	t.Helper()
	r, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestUnknownActionRejected(t *testing.T) {
	d, _ := newTest(t, &fakeAct{}, "", false)
	srv := httptest.NewServer(d.Handler())
	defer srv.Close()
	if r := post(t, srv.URL+"/api/words/x", `{"action":"dance"}`); r.StatusCode != 400 {
		t.Fatalf("status %d", r.StatusCode)
	}
	if d.settings.Words["x"].Action != "" {
		t.Fatal("action changed")
	}
	if r := post(t, srv.URL+"/api/words/x", `{"action":"louder"}`); r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	if d.settings.Words["x"].Action != "louder" {
		t.Fatal("action not saved")
	}
	if r := post(t, srv.URL+"/api/words/x", `{"action":"none"}`); r.StatusCode != 200 || d.settings.Words["x"].Action != "" {
		t.Fatal("none must clear")
	}
}

func TestEnrollTrainRejectsUnknownAction(t *testing.T) {
	d, _ := newTest(t, nil, "", false)
	srv := httptest.NewServer(d.Handler())
	defer srv.Close()
	if r := post(t, srv.URL+"/api/enroll/train", `{"action":"dance"}`); r.StatusCode != 400 {
		t.Fatalf("status %d", r.StatusCode)
	}
}
