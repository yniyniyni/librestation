package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const (
	// liveEvery is the period of the "live" event; the state and the hits
	// are looked at every stateTicks of them.
	liveEvery  = 250 * time.Millisecond
	stateTicks = 4
	// maxSubscribers bounds what forgotten tabs can cost.
	maxSubscribers = 4
	eventPing      = 25 * time.Second
	eventWriteWait = 5 * time.Second
)

// eventHub feeds GET /events. One goroutine does the work for all subscribers
// and runs only while there are some: with no client the API costs nothing.
type eventHub struct {
	mu    sync.Mutex
	subs  map[*eventSub]struct{}
	stop  chan struct{}
	wake  chan struct{} // a change just happened; see init
	first sync.Once
}

// eventSub keeps only the latest event of each kind, so a slow client loses
// stale values and never holds the hub up.
type eventSub struct {
	live bool
	wake chan struct{}

	mu      sync.Mutex
	pending map[string][]byte
	seeded  bool // got the state and the hits at least once
}

func (s *eventSub) offer(event string, data []byte) {
	s.mu.Lock()
	if s.pending == nil {
		s.pending = map[string][]byte{}
	}
	s.pending[event] = data
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *eventSub) take(event string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.pending[event]
	delete(s.pending, event)
	return data, ok
}

func (h *eventHub) init() {
	h.first.Do(func() { h.wake = make(chan struct{}, 1) })
}

// poke makes the hub look at the state now, not at its next tick.
func (h *eventHub) poke() {
	h.init()
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// addSub registers s and starts the hub if it was idle; false when full.
func (d *Daemon) addSub(s *eventSub) bool {
	h := &d.hub
	h.init()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) >= maxSubscribers {
		return false
	}
	if h.subs == nil {
		h.subs = map[*eventSub]struct{}{}
	}
	h.subs[s] = struct{}{}
	if h.stop == nil {
		h.stop = make(chan struct{})
		go d.runHub(h.stop)
	}
	h.poke()
	return true
}

func (d *Daemon) removeSub(s *eventSub) {
	h := &d.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, s)
	if len(h.subs) == 0 && h.stop != nil {
		close(h.stop)
		h.stop = nil
	}
}

func (d *Daemon) runHub(stop chan struct{}) {
	h := &d.hub
	t := time.NewTicker(liveEvery)
	defer t.Stop()
	var last snapshotJSON
	for n := 1; ; n++ {
		select {
		case <-stop:
			return
		case <-h.wake:
			d.publishSnapshot(&last)
		case <-t.C:
			d.publishLive()
			if n%stateTicks == 0 {
				d.publishSnapshot(&last)
			}
		}
	}
}

// subscribers copies the list so nothing is marshalled under h.mu.
func (h *eventHub) subscribers() []*eventSub {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*eventSub, 0, len(h.subs))
	for s := range h.subs {
		out = append(out, s)
	}
	return out
}

func (d *Daemon) publishLive() {
	var data []byte
	for _, s := range d.hub.subscribers() {
		if !s.live {
			continue
		}
		if data == nil {
			var err error
			if data, err = json.Marshal(d.live()); err != nil {
				return
			}
		}
		s.offer("live", data)
	}
}

// snapshotJSON is what subscribers last got. state is compared without its
// fast fields, which the "live" event carries.
type snapshotJSON struct{ stateKey, hits []byte }

func (d *Daemon) publishSnapshot(last *snapshotJSON) {
	st := d.state()
	hits, err := json.Marshal(map[string][]Hit{"hits": st.Hits})
	if err != nil {
		return
	}
	stateData, err := json.Marshal(st)
	if err != nil {
		return
	}
	still := st
	still.Audio.Level, still.Words = 0, append([]wordView(nil), st.Words...)
	for i := range still.Words {
		still.Words[i].Score = 0
	}
	key, err := json.Marshal(still)
	if err != nil {
		return
	}
	stateNew, hitsNew := string(key) != string(last.stateKey), string(hits) != string(last.hits)
	last.stateKey, last.hits = key, hits
	for _, s := range d.hub.subscribers() {
		s.mu.Lock()
		fresh := !s.seeded
		s.seeded = true
		s.mu.Unlock()
		if stateNew || fresh {
			s.offer("state", stateData)
		}
		if hitsNew || fresh {
			s.offer("hits", hits)
		}
	}
}

// serveEvents is GET /events: Server-Sent Events, see docs/api.md.
func (d *Daemon) serveEvents(w http.ResponseWriter, r *http.Request) {
	s := &eventSub{live: r.URL.Query().Get("live") == "1", wake: make(chan struct{}, 1)}
	if !d.addSub(s) {
		writeErr(w, &apiError{http.StatusServiceUnavailable, "too_many_subscribers", "слишком много открытых подключений"})
		return
	}
	defer d.removeSub(s)
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	write := func(text string) error {
		rc.SetWriteDeadline(time.Now().Add(eventWriteWait))
		if _, err := fmt.Fprint(w, text); err != nil {
			return err
		}
		return rc.Flush()
	}
	if write("retry: 3000\n\n") != nil {
		return
	}
	ping := time.NewTicker(eventPing)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if write(": ping\n\n") != nil {
				return
			}
		case <-s.wake:
			for _, event := range []string{"state", "hits", "live"} { // state first: it names what live refers to
				if data, ok := s.take(event); ok {
					if write("event: "+event+"\ndata: "+string(data)+"\n\n") != nil {
						return
					}
				}
			}
		}
	}
}
