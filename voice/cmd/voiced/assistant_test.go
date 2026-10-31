package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"voice/internal/agent"
	"voice/internal/daemon"
	"voice/internal/tts"
	"voice/internal/wav"
)

func TestChrootPath(t *testing.T) {
	for in, want := range map[string]string{
		"/data/local/tmp/soloist/rootfs/tmp/tts": "/tmp/tts",
		"/srv/rootfs/tmp/tts/":                   "/tmp/tts",
		"/tmp/tts":                               "/tmp/tts",
	} {
		if got := chrootPath(in); got != want {
			t.Errorf("chrootPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStockSoundsCached(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "no_internet.wav")
	if err := wav.WriteFile(f, []int16{1, 2, 3}, 22050); err != nil {
		t.Fatal(err)
	}
	s := newStockSounds(dir)
	pcm, rate, err := s.Load("no_internet")
	if err != nil || rate != 22050 || len(pcm) != 3 {
		t.Fatalf("pcm %v rate %d err %v", pcm, rate, err)
	}
	os.Remove(f)
	if pcm, _, err := s.Load("no_internet"); err != nil || len(pcm) != 3 {
		t.Fatalf("not cached: %v %v", pcm, err)
	}
	if _, _, err := s.Load("missing"); err == nil {
		t.Fatal("missing sound loaded")
	}
}

func TestTTSChoice(t *testing.T) {
	piper := &tts.Piper{}
	var set daemon.AssistantSettings
	choose := ttsChooser(func() daemon.AssistantSettings { return set }, "key", piper)
	// Piper, with Gemini behind it should Piper be missing.
	def, ok := choose().(tts.Fallback)
	if !ok || def.Primary != tts.Engine(piper) {
		t.Fatalf("default %#v", choose())
	}
	if g := def.Backup.(*tts.GeminiTTS); g.Voice != tts.DefaultVoice || g.Key != "key" {
		t.Fatalf("default backup %+v", g)
	}
	set = daemon.AssistantSettings{TTS: "gemini"}
	fb, ok := choose().(tts.Fallback)
	if !ok || fb.Backup != tts.Engine(piper) {
		t.Fatalf("gemini %#v", choose())
	}
	if g := fb.Primary.(*tts.GeminiTTS); g.Voice != tts.DefaultVoice || g.Key != "key" {
		t.Fatalf("primary %+v", g)
	}
	set.Voice = "Puck"
	if g := choose().(tts.Fallback).Primary.(*tts.GeminiTTS); g.Voice != "Puck" {
		t.Fatalf("voice %q", g.Voice)
	}
}

type errEngine struct{ err error }

func (e errEngine) Synth(context.Context, string) ([]int16, int, error) {
	return []int16{1}, 22050, e.err
}

func TestPiperHealthFollowsStartAndSynth(t *testing.T) {
	var down atomic.Bool
	h := piperHealth{Engine: errEngine{errors.New("piper: exec: no such file")}, down: &down}
	h.Synth(context.Background(), "a")
	if !down.Load() {
		t.Fatal("not down after a failed synth")
	}
	h.Engine = errEngine{}
	h.Synth(context.Background(), "a")
	if down.Load() {
		t.Fatal("still down after a good synth")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.Engine = errEngine{context.Canceled}
	h.Synth(ctx, "a")
	if down.Load() {
		t.Fatal("a cancelled synth marked Piper down")
	}
}

func TestSpotifyLinkTriState(t *testing.T) {
	var linked atomic.Bool
	calls := make(chan struct{}, 8)
	release := make(chan struct{})
	l := newSpotifyLink(func() bool { <-release; calls <- struct{}{}; return linked.Load() })
	if _, known := l.Linked(); known {
		t.Fatal("known before the first read")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go l.run(ctx, time.Hour)
	if _, known := l.Linked(); known {
		t.Fatal("known while the first read waits")
	}
	close(release)
	<-calls
	deadline := time.Now().Add(time.Second)
	for _, known := l.Linked(); !known && time.Now().Before(deadline); _, known = l.Linked() {
		time.Sleep(time.Millisecond)
	}
	if linked, known := l.Linked(); linked || !known {
		t.Fatalf("first read: linked %v known %v", linked, known)
	}
	linked.Store(true)
	l.Refresh()
	<-calls
	deadline = time.Now().Add(time.Second)
	for linked, _ := l.Linked(); !linked && time.Now().Before(deadline); linked, _ = l.Linked() {
		time.Sleep(time.Millisecond)
	}
	if linked, _ := l.Linked(); !linked {
		t.Fatal("refresh not seen")
	}
}

type stubTool struct{ ran bool }

func (s *stubTool) Decl() agent.FuncDecl { return agent.FuncDecl{Name: "stub"} }
func (s *stubTool) Run(context.Context, map[string]any) (map[string]any, error) {
	s.ran = true
	return map[string]any{"ok": true}, nil
}

func TestThenRefreshes(t *testing.T) {
	st := &stubTool{}
	n := 0
	tool := then(st, func() { n++ })
	if tool.Decl().Name != "stub" {
		t.Fatal("decl")
	}
	if _, err := tool.Run(context.Background(), nil); err != nil || !st.ran || n != 1 {
		t.Fatalf("ran %v n %d err %v", st.ran, n, err)
	}
}
