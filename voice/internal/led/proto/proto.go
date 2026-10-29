// Package proto is the datagram protocol between voiced (sender) and ledd
// (listener): small JSON events over a unix datagram socket.
package proto

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// Event is one datagram. T selects the kind: "volume", "cmd", "audio", "ai".
type Event struct {
	T     string    `json:"t"`
	V     int       `json:"v,omitempty"`
	Dir   int       `json:"dir,omitempty"`
	A     string    `json:"a,omitempty"`
	Level float64   `json:"l,omitempty"`
	Bands []float64 `json:"b,omitempty"`
	On    bool      `json:"on,omitempty"`
}

const (
	maxDatagram   = 2048
	writeDeadline = time.Millisecond
	redialEvery   = time.Second
)

// Sender sends events best-effort. Send never blocks for more than about a
// millisecond, is safe for concurrent use, and drops events on any error.
type Sender struct {
	path     string
	mu       sync.Mutex
	conn     *net.UnixConn
	lastDial time.Time
}

func NewSender(path string) *Sender { return &Sender{path: path} }

func (s *Sender) Send(e Event) {
	b, err := json.Marshal(e)
	if err != nil || len(b) > maxDatagram {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		now := time.Now()
		if !s.lastDial.IsZero() && now.Sub(s.lastDial) < redialEvery {
			return
		}
		s.lastDial = now
		addr := &net.UnixAddr{Name: s.path, Net: "unixgram"}
		c, err := net.DialUnix("unixgram", nil, addr)
		if err != nil {
			return
		}
		s.conn = c
	}
	s.conn.SetWriteDeadline(time.Now().Add(writeDeadline))
	if _, err := s.conn.Write(b); err != nil {
		s.conn.Close()
		s.conn = nil
	}
}

// Listen binds the socket at path (replacing a stale file), makes it
// world-writable and calls onEvent for every valid datagram.
func Listen(path string, onEvent func(Event)) (io.Closer, error) {
	os.Remove(path)
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o666); err != nil {
		conn.Close()
		return nil, err
	}
	go func() {
		buf := make([]byte, maxDatagram)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					continue
				}
				return
			}
			var e Event
			if json.Unmarshal(buf[:n], &e) != nil {
				continue
			}
			onEvent(e)
		}
	}()
	return conn, nil
}
