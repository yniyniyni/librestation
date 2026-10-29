package main

import (
	"errors"
	"testing"
)

type fakePlayer struct{ playing []bool }

func (f *fakePlayer) Playing(on bool) { f.playing = append(f.playing, on) }

func TestSoloistSyncReconnectWithinOneTick(t *testing.T) {
	sc := &fakePlayer{}
	sent := 0
	send := func() error { sent++; return nil }
	s := &soloistSync{sc: sc, send: send}
	s.step(true, 1) // first connect
	if sent != 1 {
		t.Fatalf("sent %d after first connect", sent)
	}
	s.step(true, 1)
	if sent != 1 {
		t.Fatalf("resent without reconnect: %d", sent)
	}
	s.step(true, 2) // dropped and reconnected between ticks
	if sent != 2 {
		t.Fatalf("missed fast reconnect: sent %d", sent)
	}
}

func TestSoloistSyncRetriesFailedGetState(t *testing.T) {
	sc := &fakePlayer{}
	fail := true
	sent := 0
	s := &soloistSync{sc: sc, send: func() error {
		sent++
		if fail {
			return errors.New("not connected")
		}
		return nil
	}}
	s.step(true, 1)
	s.step(true, 1)
	fail = false
	s.step(true, 1)
	s.step(true, 1)
	if sent != 3 {
		t.Fatalf("sent %d, want 3 (two failures then success, then idle)", sent)
	}
}

func TestSoloistSyncDisconnectStopsPlayback(t *testing.T) {
	sc := &fakePlayer{}
	s := &soloistSync{sc: sc, send: func() error { return nil }}
	s.step(false, 0)
	if len(sc.playing) != 1 || sc.playing[0] {
		t.Fatalf("%v", sc.playing)
	}
	sent := 0
	s.send = func() error { sent++; return nil }
	s.step(true, 1)
	if sent != 1 {
		t.Fatalf("sent %d after connect", sent)
	}
}

func TestEnsureSafeBacklight(t *testing.T) {
	d := t.TempDir()
	bl := d + "/b"
	if err := writeFile(bl); err != nil {
		t.Fatal(err)
	}
	lux := d + "/lux"
	// unreadable lux -> darkest level
	safeBacklight(hwPathsFor(bl, lux))
	if got := readFile(t, bl); got != "3" {
		t.Fatalf("no lux: %q, want 3", got)
	}
	writeLux(t, lux, "200")
	safeBacklight(hwPathsFor(bl, lux))
	if got := readFile(t, bl); got != "100" {
		t.Fatalf("bright: %q, want 100", got)
	}
}
