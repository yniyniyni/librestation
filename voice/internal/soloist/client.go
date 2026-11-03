// Package soloist is a client for the Soloist headless Spotify client's
// local WebSocket API (ws://127.0.0.1:9771).
package soloist

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

// EventConnected is the Type of the event sent after each (re)connect: Soloist
// may have restarted, with its start values.
const EventConnected = "connected"

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
	gen    uint64 // number of connections established so far
	vol    int
	volSet bool

	context string // what Soloist last reported playing
	item    string
	status  string
	shuffle bool
	lastErr string // its last error message, since clearErr
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
			c.gen++
			c.mu.Unlock()
			if c.onEvent != nil {
				c.onEvent(Event{Type: EventConnected})
			}
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
	Type    string `json:"type"`
	Volume  *int   `json:"volume"`
	Status  string `json:"status"`
	Message string `json:"message"`
	Context *struct {
		URI string `json:"uri"`
	} `json:"context"`
	Item *struct {
		URI string `json:"uri"`
	} `json:"item"`
	Options *struct {
		Shuffle bool `json:"shuffle"`
	} `json:"options"`
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
		c.note(m)
		if c.onEvent != nil {
			c.onEvent(Event{Type: m.Type, Volume: m.Volume, Status: m.Status})
		}
	}
}

// note keeps what the commands below wait for.
func (c *Client) note(m wireMsg) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if m.Type == "error" {
		c.lastErr = m.Message
	}
	if m.Type != "playback_state" {
		return
	}
	c.status = m.Status
	if m.Context != nil {
		c.context = m.Context.URI
	}
	c.item = ""
	if m.Item != nil {
		c.item = m.Item.URI
	}
	if m.Options != nil {
		c.shuffle = m.Options.Shuffle
	}
}

// Connected reports whether a connection is currently up.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

// Generation counts the connections made so far; it changes on every
// (re)connect, even one too short for Connected polling to notice.
func (c *Client) Generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
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

const (
	// waitStep is how often a command's effect is looked for.
	waitStep = 20 * time.Millisecond
	// radioStart bounds the wait for a station to load, radioTrack and
	// shuffleWait for the commands that follow.
	radioStart  = 5 * time.Second
	radioTrack  = 1500 * time.Millisecond
	radioSkips  = 4
	shuffleWait = 1500 * time.Millisecond
)

// wait polls ok until it holds or d or ctx ends. Reports whether it held; ok
// runs under c.mu.
func (c *Client) wait(ctx context.Context, d time.Duration, ok func() bool) bool {
	end := time.Now().Add(d)
	for {
		c.mu.Lock()
		done := ok()
		c.mu.Unlock()
		if done {
			return true
		}
		if time.Now().After(end) || ctx.Err() != nil {
			return false
		}
		select {
		case <-ctx.Done():
		case <-time.After(waitStep):
		}
	}
}

func (c *Client) clearErr() {
	c.mu.Lock()
	c.lastErr = ""
	c.mu.Unlock()
}

// PlayRadio plays a track and then Spotify's station of it. A station only
// exists as a context of its own, and Soloist's play takes it where the Web API
// does not; it starts on a track of its choosing, so the one asked for goes
// through the queue, in front of the rest.
func (c *Client) PlayRadio(ctx context.Context, track string) error {
	id, ok := strings.CutPrefix(track, "spotify:track:")
	if !ok || id == "" {
		return fmt.Errorf("soloist: не трек: %q", track)
	}
	station := "spotify:station:track:" + id
	c.clearErr()
	if err := c.send(struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		URI     string `json:"uri"`
	}{"command", "play", station}); err != nil {
		return err
	}
	var failed string
	started := c.wait(ctx, radioStart, func() bool {
		failed = c.lastErr
		return c.context == station || failed != ""
	})
	if failed != "" {
		return fmt.Errorf("soloist: %s", failed)
	}
	if !started {
		return fmt.Errorf("soloist: станция не запустилась")
	}
	if err := c.send(struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		URI     string `json:"uri"`
	}{"command", "add_to_queue", track}); err != nil {
		return err
	}
	// It reports playing at once, with no item, then loads its first track: a
	// skip before that track plays is lost, or skips past the queue.
	if !c.wait(ctx, radioStart, func() bool { return c.status == "playing" && c.item != "" }) {
		return fmt.Errorf("soloist: станция не запустилась")
	}
	// Tracks queued earlier come first, so it may take more than one skip.
	for range radioSkips {
		if err := c.Next(); err != nil {
			return err
		}
		if c.wait(ctx, radioTrack, func() bool { return c.item == track }) {
			return nil
		}
	}
	return fmt.Errorf("soloist: трек не начался")
}

// SetShuffle turns shuffle on or off and waits for Soloist to report it: what
// is playing may not allow it, a station does not.
func (c *Client) SetShuffle(ctx context.Context, on bool) error {
	if err := c.send(struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Enabled bool   `json:"enabled"`
	}{"command", "set_shuffle", on}); err != nil {
		return err
	}
	if !c.wait(ctx, shuffleWait, func() bool { return c.shuffle == on }) {
		return fmt.Errorf("то, что сейчас играет, нельзя перемешать")
	}
	return nil
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
