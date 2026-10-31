package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"voice/internal/agent"
	"voice/internal/assistant"
	"voice/internal/daemon"
	"voice/internal/geminilive"
	"voice/internal/led/proto"
	"voice/internal/spotifyapi"
	"voice/internal/tools"
	"voice/internal/tts"
	"voice/internal/volume"
	"voice/internal/wav"
)

// assistantConfig are the assistant's flags.
type assistantConfig struct {
	spotifyToken, spotifyDevice string
	speakerCmd                  string
	piperCmd, piperOut          string
	sounds                      string
	liveModel                   string
}

// spotifyLinkEvery is how often the panel's Spotify status is re-read.
const spotifyLinkEvery = 30 * time.Second

// piperWarmDelay holds the Piper warm-up back after voiced starts: at boot
// chroot.sh's mounts race with Soloist's.
const piperWarmDelay = 5 * time.Second

type builtAssistant struct {
	session   *assistant.Session
	speaker   *speaker
	link      *spotifyLink
	piperDown *atomic.Bool // Piper did not start or its last synthesis failed
}

// buildAssistant wires the session: the agent with its tools, Piper (started
// now, in the background) or Gemini speech, the reply speaker and ducking.
// model is the model for each turn; judgeModel is the client's fallback.
func buildAssistant(ctx context.Context, c assistantConfig, key, judgeModel string, model func() string,
	settings func() daemon.AssistantSettings,
	act daemon.Actuator, vol *volume.Sync, out *output, led func(proto.Event)) builtAssistant {
	sp := spotifyapi.New(c.spotifyToken, c.spotifyDevice)
	link := newSpotifyLink(sp.Linked)
	go link.run(ctx, spotifyLinkEvery)
	music := func(t agent.Tool) agent.Tool { return then(t, link.Refresh) }

	llm := agent.NewClient(key)
	llm.Model = judgeModel
	ag := &agent.Agent{Model: model, LLM: llm, Tools: tools.All(tools.Deps{
		Spotify:     sp,
		Actuator:    act,
		SetVolume:   func(v int) error { return vol.Step(v - vol.Volume()) },
		DefaultCity: func() string { return settings().City },
		LLM:         llm,
		Model:       model,
		Music:       music,
	})}

	if err := os.MkdirAll(c.piperOut, 0o755); err != nil {
		log.Printf("piper: %v", err)
	}
	piper := &tts.Piper{Cmd: strings.Fields(c.piperCmd), OutDir: chrootPath(c.piperOut), HostOutDir: c.piperOut}
	piperDown := new(atomic.Bool)
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(piperWarmDelay):
		}
		if err := piper.Start(ctx); err != nil {
			piperDown.Store(true)
			log.Printf("piper: %v (replies will use Gemini's voice)", err)
		}
	}()

	spk := newSpeaker(strings.Fields(c.speakerCmd), func() { out.refresh(vol.Volume()) })
	sess := assistant.New(assistant.Deps{
		Agent:  ag,
		TTS:    ttsChooser(settings, key, piperHealth{piper, piperDown}),
		Player: spk,
		Duck:   out.duck,
		LED:    led,
		Stock:  newStockSounds(c.sounds).Load,
		Live: func(lctx context.Context) (assistant.LiveSession, error) {
			return geminilive.Dial(lctx, geminilive.Config{
				Key: key, Model: c.liveModel, Voice: settings().Voice,
				Prompt: assistant.LivePrompt, Tools: assistant.LiveDeclarations(ag.Tools),
			})
		},
	})
	log.Printf("assistant: on, Live model %s; text fallback model %s", c.liveModel, model())
	return builtAssistant{sess, spk, link, piperDown}
}

// piperHealth notes whether Piper's last synthesis worked, for the panel.
type piperHealth struct {
	tts.Engine
	down *atomic.Bool
}

func (h piperHealth) Synth(ctx context.Context, text string) ([]int16, int, error) {
	pcm, rate, err := h.Engine.Synth(ctx, text)
	if ctx.Err() == nil {
		h.down.Store(err != nil)
	}
	return pcm, rate, err
}

// chrootPath is a path under the chroot's rootfs as seen inside it.
func chrootPath(host string) string {
	host = filepath.Clean(host)
	if i := strings.Index(host, "/rootfs/"); i >= 0 {
		return host[i+len("/rootfs"):]
	}
	return host
}

// ttsChooser picks the engine for a turn from the panel: Gemini with Piper
// (offline) behind it, the default, or Piper with Gemini behind it.
func ttsChooser(settings func() daemon.AssistantSettings, key string, piper tts.Engine) func() tts.Engine {
	return func() tts.Engine {
		s := settings()
		g := tts.NewGeminiTTS(key)
		if s.Voice != "" {
			g.Voice = s.Voice
		}
		if s.TTS == "piper" {
			return tts.Fallback{Primary: piper, Backup: g}
		}
		return tts.Fallback{Primary: g, Backup: piper}
	}
}

// stockSounds reads <dir>/<name>.wav at its own rate, once per name.
type stockSounds struct {
	dir   string
	mu    sync.Mutex
	cache map[string]stockSound
}

type stockSound struct {
	pcm  []int16
	rate int
}

func newStockSounds(dir string) *stockSounds {
	return &stockSounds{dir: dir, cache: map[string]stockSound{}}
}

func (s *stockSounds) Load(name string) ([]int16, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.cache[name]; ok {
		return c.pcm, c.rate, nil
	}
	b, err := os.ReadFile(filepath.Join(s.dir, name+".wav"))
	if err != nil {
		return nil, 0, err
	}
	pcm, rate, err := wav.Decode(b)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", name, err)
	}
	s.cache[name] = stockSound{pcm, rate}
	return pcm, rate, nil
}

// spotifyLink caches whether Spotify is linked for the panel: Linked may wait
// on a token refresh, and the state API must answer at once.
type spotifyLink struct {
	linked func() bool
	state  atomic.Int32 // linkUnknown until the first read
	kick   chan struct{}
}

const (
	linkUnknown int32 = iota
	linkNo
	linkYes
)

func newSpotifyLink(linked func() bool) *spotifyLink {
	return &spotifyLink{linked: linked, kick: make(chan struct{}, 1)}
}

// Linked is the last value read; known is false before the first read ends.
func (l *spotifyLink) Linked() (linked, known bool) {
	st := l.state.Load()
	return st == linkYes, st != linkUnknown
}

// Refresh asks for a re-read without waiting for it.
func (l *spotifyLink) Refresh() {
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

// run reads the value now, every interval and on Refresh, until ctx ends.
func (l *spotifyLink) run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		st := linkNo
		if l.linked() {
			st = linkYes
		}
		l.state.Store(st)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-l.kick:
		}
	}
}

// then runs after once each call of t has finished (a music call may link or
// unlink Spotify).
func then(t agent.Tool, after func()) agent.Tool { return thenTool{t, after} }

type thenTool struct {
	agent.Tool
	after func()
}

func (t thenTool) Run(ctx context.Context, args map[string]any) (map[string]any, error) {
	defer t.after()
	return t.Tool.Run(ctx, args)
}
