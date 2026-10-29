package soloist

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

type fakeServer struct {
	srv   *httptest.Server
	conns chan *websocket.Conn
	msgs  chan string
}

func newFake(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{conns: make(chan *websocket.Conn, 8), msgs: make(chan string, 32)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		f.conns <- c
		for {
			_, b, err := c.Read(context.Background())
			if err != nil {
				return
			}
			f.msgs <- string(b)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeServer) url() string { return "ws" + strings.TrimPrefix(f.srv.URL, "http") }

func (f *fakeServer) conn(t *testing.T) *websocket.Conn {
	t.Helper()
	select {
	case c := <-f.conns:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no connection")
		return nil
	}
}

func (f *fakeServer) msg(t *testing.T) string {
	t.Helper()
	select {
	case m := <-f.msgs:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("no message")
		return ""
	}
}

func waitConnected(t *testing.T, c *Client) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.Connected() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("client never connected")
}

func dial(t *testing.T, url string, on func(Event)) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	c := Dial(ctx, url, on)
	t.Cleanup(func() { cancel(); c.Close() })
	return c
}

func TestSendsPause(t *testing.T) {
	f := newFake(t)
	c := dial(t, f.url(), nil)
	f.conn(t)
	waitConnected(t, c)
	if err := c.Pause(); err != nil {
		t.Fatal(err)
	}
	if got := f.msg(t); got != `{"type":"command","command":"pause"}` {
		t.Fatalf("got %s", got)
	}
}

func TestAdapterCommands(t *testing.T) {
	f := newFake(t)
	c := dial(t, f.url(), nil)
	f.conn(t)
	waitConnected(t, c)
	for name, fn := range map[string]func() error{"play": c.Play, "skip_next": c.Next, "skip_prev": c.Prev} {
		if err := fn(); err != nil {
			t.Fatal(err)
		}
		if got, want := f.msg(t), `{"type":"command","command":"`+name+`"}`; got != want {
			t.Fatalf("got %s want %s", got, want)
		}
	}
}

func TestSetVolumeClamps(t *testing.T) {
	f := newFake(t)
	c := dial(t, f.url(), nil)
	f.conn(t)
	waitConnected(t, c)
	for in, want := range map[int]int{-5: 0, 130: 100, 42: 42} {
		if err := c.SetVolume(in); err != nil {
			t.Fatal(err)
		}
		exp := `{"type":"command","command":"set_volume","volume":` + itoa(want) + `}`
		if got := f.msg(t); got != exp {
			t.Fatalf("got %s want %s", got, exp)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for ; n > 0; n /= 10 {
		s = string(rune('0'+n%10)) + s
	}
	return s
}

func TestNotConnectedErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := Dial(ctx, "ws://127.0.0.1:1", nil)
	defer c.Close()
	done := make(chan error, 1)
	go func() { done <- c.Pause() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want error")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked")
	}
	if err := c.SetVolume(5); err == nil {
		t.Fatal("want error")
	}
}

func TestVolumeFromEvents(t *testing.T) {
	f := newFake(t)
	evs := make(chan Event, 8)
	c := dial(t, f.url(), func(e Event) { evs <- e })
	sc := f.conn(t)
	if _, ok := c.Volume(); ok {
		t.Fatal("volume known too early")
	}
	ctx := context.Background()
	sc.Write(ctx, websocket.MessageText, []byte(`{"type":"playback_state","status":"playing","volume":65,"is_active":true}`))
	e := <-evs
	if e.Type != "playback_state" || e.Status != "playing" || e.Volume == nil || *e.Volume != 65 {
		t.Fatalf("event %+v", e)
	}
	if v, ok := c.Volume(); !ok || v != 65 {
		t.Fatalf("vol %d %v", v, ok)
	}
	sc.Write(ctx, websocket.MessageText, []byte(`{"type":"volume_changed","volume":42}`))
	<-evs
	if v, ok := c.Volume(); !ok || v != 42 {
		t.Fatalf("vol %d %v", v, ok)
	}
	// playback_state without volume leaves it alone
	sc.Write(ctx, websocket.MessageText, []byte(`{"type":"playback_changed","status":"paused"}`))
	e = <-evs
	if e.Status != "paused" || e.Volume != nil {
		t.Fatalf("event %+v", e)
	}
	if v, _ := c.Volume(); v != 42 {
		t.Fatalf("vol %d", v)
	}
}

func TestClientReconnects(t *testing.T) {
	f := newFake(t)
	c := dial(t, f.url(), nil)
	sc := f.conn(t)
	waitConnected(t, c)
	sc.Close(websocket.StatusGoingAway, "bye")
	f.conn(t) // second connection
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := c.Next(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never reconnected")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := f.msg(t); got != `{"type":"command","command":"skip_next"}` {
		t.Fatalf("got %s", got)
	}
}

func TestErrorEventIsNotFatal(t *testing.T) {
	f := newFake(t)
	evs := make(chan Event, 8)
	c := dial(t, f.url(), func(e Event) { evs <- e })
	sc := f.conn(t)
	waitConnected(t, c)
	sc.Write(context.Background(), websocket.MessageText, []byte(`{"type":"error","message":"not logged in"}`))
	if e := <-evs; e.Type != "error" {
		t.Fatalf("event %+v", e)
	}
	sc.Write(context.Background(), websocket.MessageText, []byte(`garbage`))
	if err := c.Pause(); err != nil {
		t.Fatal(err)
	}
	f.msg(t)
	select {
	case <-f.conns:
		t.Fatal("reconnected after error event")
	case <-time.After(1500 * time.Millisecond):
	}
}
