package proto

import (
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func sockPath(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "lp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return filepath.Join(d, "s")
}

type collector struct {
	mu sync.Mutex
	ev []Event
}

func (c *collector) add(e Event) { c.mu.Lock(); c.ev = append(c.ev, e); c.mu.Unlock() }
func (c *collector) get() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Event(nil), c.ev...)
}

func waitFor(t *testing.T, d time.Duration, f func() bool) bool {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if f() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return f()
}

func TestRoundTrip(t *testing.T) {
	p := sockPath(t)
	c := &collector{}
	l, err := Listen(p, c.add)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o666 {
		t.Fatalf("socket mode: %v %v", st, err)
	}
	want := []Event{
		{T: "volume", V: 42, Dir: -1},
		{T: "cmd", A: "play"},
		{T: "audio", Level: 0.5, Bands: []float64{0.1, 0.2, 0.3}},
		{T: "ai", On: true},
	}
	s := NewSender(p)
	for _, e := range want {
		s.Send(e)
		time.Sleep(2 * time.Millisecond)
	}
	if !waitFor(t, time.Second, func() bool { return len(c.get()) >= len(want) }) {
		t.Fatalf("got %v", c.get())
	}
	if got := c.get(); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

func TestSendWithoutListenerDoesNotBlock(t *testing.T) {
	s := NewSender(sockPath(t))
	start := time.Now()
	for i := 0; i < 1000; i++ {
		s.Send(Event{T: "volume", V: i})
	}
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("took %v", d)
	}
}

func TestSendAfterListenerRestart(t *testing.T) {
	p := sockPath(t)
	c1 := &collector{}
	l, err := Listen(p, c1.add)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSender(p)
	s.Send(Event{T: "ai", On: true})
	if !waitFor(t, time.Second, func() bool { return len(c1.get()) == 1 }) {
		t.Fatal("first listener got nothing")
	}
	l.Close()
	c2 := &collector{}
	l2, err := Listen(p, c2.add)
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	ok := waitFor(t, 1500*time.Millisecond, func() bool {
		s.Send(Event{T: "cmd", A: "pause"})
		return len(c2.get()) > 0
	})
	if !ok {
		t.Fatal("no events after restart")
	}
}

func TestBadJSONIgnored(t *testing.T) {
	p := sockPath(t)
	c := &collector{}
	l, err := Listen(p, c.add)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	s := NewSender(p)
	s.Send(Event{T: "ai", On: true}) // dial
	if !waitFor(t, time.Second, func() bool { return len(c.get()) == 1 }) {
		t.Fatal("setup failed")
	}
	// raw garbage datagram
	conn, err := dialRaw(p)
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte("{not json"))
	conn.Close()
	s.Send(Event{T: "cmd", A: "next"})
	if !waitFor(t, time.Second, func() bool { return len(c.get()) == 2 }) {
		t.Fatalf("got %v", c.get())
	}
	if c.get()[1].A != "next" {
		t.Fatalf("got %v", c.get())
	}
}

func dialRaw(p string) (net.Conn, error) { return net.Dial("unixgram", p) }
