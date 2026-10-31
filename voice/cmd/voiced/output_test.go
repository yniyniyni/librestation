package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

type lines struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// The audio HAL resets the amplifier when it wakes from standby; refresh
// writes the same volume again although the helper was already given it.
func TestRefreshRewritesSameVolume(t *testing.T) {
	o := newOutput(nil, "")
	var w lines
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { o.ramp(ctx, &w, -1); close(done) }()
	defer func() { cancel(); <-done }()

	wait := func(want string) {
		t.Helper()
		for range 200 {
			if w.String() == want {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("helper got %q, want %q", w.String(), want)
	}
	o.set(32)
	wait("32\n")
	o.set(32) // unchanged: nothing to write
	time.Sleep(30 * time.Millisecond)
	wait("32\n")
	o.refresh(32)
	wait("32\n32\n")
}
