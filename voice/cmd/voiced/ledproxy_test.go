package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"voice/internal/led/proto"
)

func TestLEDProxyForwards(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Write([]byte(r.Method + " " + r.URL.Path + " " + string(b)))
	}))
	defer up.Close()
	h, err := ledProxy(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	r, err := http.Post(srv.URL+"/api/led/ai", "application/json", strings.NewReader(`{"on":true}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	if string(b) != `POST /api/led/ai {"on":true}` {
		t.Fatalf("got %q", b)
	}
}

func TestLEDProxyDownIs502JSON(t *testing.T) {
	up := httptest.NewServer(http.NotFoundHandler())
	url := up.URL
	up.Close()
	h, _ := ledProxy(url)
	srv := httptest.NewServer(h)
	defer srv.Close()
	r, err := http.Get(srv.URL + "/api/led")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 502 || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") || !strings.Contains(string(b), `"error"`) {
		t.Fatalf("status %d type %q body %s", r.StatusCode, r.Header.Get("Content-Type"), b)
	}
}

func TestOutputSendsVolumeEvents(t *testing.T) {
	var got []proto.Event
	o := newOutput(nil, "")
	o.led = func(e proto.Event) { got = append(got, e) }
	o.set(30)
	o.set(45)
	o.set(40)
	wantV, wantDir := []int{30, 45, 40}, []int{0, 1, -1}
	if len(got) != 3 {
		t.Fatalf("got %+v", got)
	}
	for i, e := range got {
		if e.T != "volume" || e.V != wantV[i] || e.Dir != wantDir[i] {
			t.Fatalf("event %d: %+v", i, e)
		}
	}
}
