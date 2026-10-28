// Package soloist is a client for the Soloist headless Spotify client's
// local WebSocket API (ws://127.0.0.1:9771).
package soloist

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Event is a server-pushed message. Volume is nil when the message has none.
type Event struct {
	Type   string
	Volume *int
	Status string
}

// ErrNotConnected is returned when no WebSocket connection is up.
var ErrNotConnected = errors.New("soloist: not connected")

const (
	minBackoff   = time.Second
	maxBackoff   = 10 * time.Second
	writeTimeout = 2 * time.Second
)

// Client keeps a reconnecting connection to Soloist.
type Client struct {
	url     string
	onEvent func(Event)
	cancel  context.CancelFunc
	done    chan struct{}

	mu     sync.Mutex
	conn   *websocket.Conn
	vol    int
	volSet bool
}

// Dial starts connecting in the background and returns immediately. The
// client reconnects with 1 s to 10 s backoff until ctx is cancelled or Close.
func Dial(ctx context.Context, url string, onEvent func(Event)) *Client {
	ctx, cancel := context.WithCancel(ctx)
	c := &Client{url: url, onEvent: onEvent, cancel: cancel, done: make(chan struct{})}
	go c.run(ctx)
	return c
}

// Close stops the client and waits for the background loop to exit.
func (c *Client) Close() {
	c.cancel()
	<-c.done
}

func (c *Client) run(ctx context.Context) {
	defer close(c.done)
	backoff := minBackoff
	for ctx.Err() == nil {
		start := time.Now()
		conn, _, err := websocket.Dial(ctx, c.url, nil)
		if err == nil {
			conn.SetReadLimit(1 << 20)
			c.mu.Lock()
			c.conn = conn
			c.mu.Unlock()
			c.readLoop(ctx, conn)
			c.mu.Lock()
			c.conn = nil
			c.mu.Unlock()
			conn.CloseNow()
			if time.Since(start) > maxBackoff {
				backoff = minBackoff
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

type wireMsg struct {
	Type   string `json:"type"`
	Volume *int   `json:"volume"`
	Status string `json:"status"`
}

func (c *Client) readLoop(ctx context.Context, conn *websocket.Conn) {
	for {
		_, b, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var m wireMsg
		if json.Unmarshal(b, &m) != nil || m.Type == "" {
			continue // ignore malformed frames; the connection stays up
		}
		if m.Volume != nil && (m.Type == "playback_state" || m.Type == "volume_changed") {
			c.mu.Lock()
			c.vol, c.volSet = *m.Volume, true
			c.mu.Unlock()
		}
		if c.onEvent != nil {
			c.onEvent(Event{Type: m.Type, Volume: m.Volume, Status: m.Status})
		}
	}
}

// Connected reports whether a connection is currently up.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

func (c *Client) send(v any) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return ErrNotConnected
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, b)
}

// Command sends a bare command (play, pause, skip_next, skip_prev, get_state).
// It returns once the frame is written; Soloist replies asynchronously.
func (c *Client) Command(name string) error {
	return c.send(struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}{"command", name})
}

// SetVolume sends set_volume, clamping v to 0..100.
func (c *Client) SetVolume(v int) error {
	v = min(max(v, 0), 100)
	return c.send(struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Volume  int    `json:"volume"`
	}{"command", "set_volume", v})
}

// Volume returns the last volume reported by Soloist.
func (c *Client) Volume() (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.vol, c.volSet
}

// Pause, Play, Next and Prev implement the transport part of daemon.Actuator.
func (c *Client) Pause() error { return c.Command("pause") }
func (c *Client) Play() error  { return c.Command("play") }
func (c *Client) Next() error  { return c.Command("skip_next") }
func (c *Client) Prev() error  { return c.Command("skip_prev") }
