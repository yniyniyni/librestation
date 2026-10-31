package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"voice/internal/agent"
	"voice/internal/assistant"
	"voice/internal/daemon"
	"voice/internal/input"
	"voice/internal/judge"
	"voice/internal/led/proto"
	"voice/internal/soloist"
	"voice/internal/spotifyapi"
	"voice/internal/tools"
	"voice/internal/tts"
	"voice/internal/volume"
	"voice/internal/wav"
)

func cmdRun(args []string) error {
	exe, _ := os.Executable()
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var opt daemon.Options
	var micCmd, hwCmd, gainFile, knob, ledSock, leddURL, judgeKey, judgeModel string
	var ac assistantConfig
	fs.StringVar(&opt.DataDir, "data", "/data/local/tmp/voice/data", "words and settings")
	fs.StringVar(&micCmd, "mic-cmd", "sh /data/local/tmp/voice/mic.sh", "mic capture helper command")
	fs.StringVar(&opt.Soloist, "soloist", "ws://127.0.0.1:9771", "Soloist WebSocket URL")
	fs.StringVar(&opt.Negatives, "negatives", filepath.Join(filepath.Dir(exe), "negatives.bin"), "bundled negative set")
	fs.StringVar(&opt.Listen, "listen", ":8780", "web UI address")
	fs.StringVar(&hwCmd, "hw-cmd", "sh /data/local/tmp/voice/hwvol.sh -", "amplifier helper reading 0..100 per stdin line; empty = leave it")
	fs.StringVar(&gainFile, "gain-file", "/data/local/tmp/soloist/gain", "aaudio_pipe gain that undoes Soloist's software volume; empty = none")
	fs.StringVar(&knob, "knob", "/dev/input/event2", "volume knob evdev device; empty = none")
	fs.StringVar(&ledSock, "led-sock", "/data/local/tmp/voice/ledd.sock", "ledd event socket; empty = no LED events")
	fs.StringVar(&leddURL, "ledd", "http://127.0.0.1:8781", "ledd HTTP API behind /api/led")
	fs.StringVar(&judgeKey, "judge-key", "/data/local/tmp/voice/gemini.key", "Gemini API key file; missing = hits are not judged")
	fs.StringVar(&judgeModel, "judge-model", judge.DefaultModel, "Gemini model that judges hits")
	fs.StringVar(&ac.spotifyToken, "spotify-token", "/data/local/tmp/voice/spotify.json", "Spotify refresh token file written by setup")
	fs.StringVar(&ac.spotifyDevice, "spotify-device", "", "this speaker's Spotify Connect name; empty = the active device")
	fs.StringVar(&ac.speakerCmd, "speaker-cmd", "/data/local/tmp/soloist/aaudio_pipe 24000", "helper that plays replies (float32 stereo on stdin)")
	fs.StringVar(&ac.piperCmd, "piper-cmd", "sh /data/local/tmp/soloist/chroot.sh /opt/piper/piper --model /opt/piper/voice.onnx --json-input", "resident Piper command")
	fs.StringVar(&ac.piperOut, "piper-out", "/data/local/tmp/soloist/rootfs/tmp/tts", "where Piper writes, as voiced sees it (inside the chroot's rootfs)")
	fs.StringVar(&ac.sounds, "sounds", "/data/local/tmp/voice/sounds", "stock phrases <name>.wav")
	fs.Parse(args)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	opt.MicCmd = strings.Fields(micCmd)
	useAndroidNet()
	key, err := judge.ReadKey(judgeKey)
	if err != nil {
		log.Printf("judge: %v; hits are not judged, no assistant", err)
		key = ""
	} else if key != "" {
		jc := judge.New(key)
		jc.Model = judgeModel
		opt.Judge, opt.JudgeModel = jc, judgeModel
		log.Printf("judge: hits are labelled by %s", judgeModel)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Soloist's volume is the master (see package volume); the amplifier
	// follows it and aaudio_pipe undoes Soloist's own attenuation.
	out := newOutput(strings.Fields(hwCmd), gainFile)
	var ledSend func(proto.Event)
	if ledSock != "" {
		sender := proto.NewSender(ledSock)
		ledSend = sender.Send
		out.led = sender.Send
		opt.OnLED = sender.Send
	}
	go out.run(ctx)
	// The daemon watches playback and volume changes from every source to
	// tell when the user takes a voice command back; it is created last.
	var dmn atomic.Pointer[daemon.Daemon]
	var vol *volume.Sync
	volReady := make(chan struct{})
	sol := soloist.Dial(ctx, opt.Soloist, func(e soloist.Event) {
		if d := dmn.Load(); d != nil && (e.Type == "playback_state" || e.Type == "playback_changed") {
			d.ObservePlayback(e.Status)
		}
		select {
		case <-volReady:
			vol.OnEvent(e)
		default:
		}
	})
	defer sol.Close()
	vol = volume.New(sol, filepath.Join(opt.DataDir, "volume"))
	vol.Out = func(v int) {
		out.set(v)
		if d := dmn.Load(); d != nil {
			d.ObserveVolume(v)
		}
	}
	vol.Resumed = out.refresh
	close(volReady)
	go vol.Run(ctx)
	if knob != "" {
		go readKnob(ctx, knob, vol.Key)
	}
	opt.Actuator = actuator{sol, vol}

	// The session reads the panel's settings on every turn.
	settings := func() daemon.AssistantSettings {
		if d := dmn.Load(); d != nil {
			return d.AssistantSettings()
		}
		return daemon.AssistantSettings{}
	}
	if key == "" {
		log.Printf("assistant: no Gemini key, off")
	} else {
		a := buildAssistant(ctx, ac, key, judgeModel, settings, opt.Actuator, vol, out, ledSend)
		defer a.speaker.Close()
		opt.Assistant = a.session
		opt.AssistantStatus = func() any { return a.session.Status() }
		opt.SpotifyLinked = a.link.Linked
	}

	d, err := daemon.New(opt)
	if err != nil {
		return err
	}
	dmn.Store(d)

	led, err := ledProxy(leddURL)
	if err != nil {
		return fmt.Errorf("-ledd: %w", err)
	}
	mux := withVolumeAPI(d.Handler(), vol)
	mux.Handle("/api/led", led)
	mux.Handle("/api/led/", led)
	srv := &http.Server{Addr: opt.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	go d.Run(ctx)
	log.Printf("voiced: UI on %s, data in %s", opt.Listen, opt.DataDir)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// actuator sends transport commands to Soloist and volume steps to the
// unified volume.
type actuator struct {
	*soloist.Client
	vol *volume.Sync
}

var _ daemon.Actuator = actuator{}

func (a actuator) Step(delta int) error { return a.vol.Step(delta) }

// withVolumeAPI adds /api/volume (current volume and knob step) to the UI.
func withVolumeAPI(h http.Handler, vol *volume.Sync) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/", h)
	state := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]int{"volume": vol.Volume(), "knob_step": vol.KnobStep()})
	}
	mux.HandleFunc("GET /api/volume", func(w http.ResponseWriter, r *http.Request) { state(w) })
	mux.HandleFunc("POST /api/volume", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			KnobStep int `json:"knob_step"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
			return
		}
		if err := vol.SetKnobStep(req.KnobStep); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
			return
		}
		state(w)
	})
	return mux
}

// readKnob reads knob presses, reopening the device after errors.
func readKnob(ctx context.Context, path string, onKey func(uint16)) {
	for ctx.Err() == nil {
		err := input.ReadKeys(ctx, path, onKey)
		if ctx.Err() != nil {
			return
		}
		log.Printf("knob %s: %v", path, err)
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}

// assistantConfig are the assistant's flags.
type assistantConfig struct {
	spotifyToken, spotifyDevice string
	speakerCmd                  string
	piperCmd, piperOut          string
	sounds                      string
}

// spotifyLinkEvery is how often the panel's Spotify status is re-read.
const spotifyLinkEvery = 30 * time.Second

type builtAssistant struct {
	session *assistant.Session
	speaker *speaker
	link    *spotifyLink
}

// buildAssistant wires the session: the agent with its tools, Piper (started
// now, in the background) or Gemini speech, the reply speaker and ducking.
func buildAssistant(ctx context.Context, c assistantConfig, key, model string, settings func() daemon.AssistantSettings,
	act daemon.Actuator, vol *volume.Sync, out *output, led func(proto.Event)) builtAssistant {
	sp := spotifyapi.New(c.spotifyToken, c.spotifyDevice)
	link := newSpotifyLink(sp.Linked)
	go link.run(ctx, spotifyLinkEvery)
	music := func(t agent.Tool) agent.Tool { return then(t, link.Refresh) }

	llm := agent.NewClient(key)
	llm.Model = model
	modelFn := func() string { return settings().Model }
	ag := &agent.Agent{Model: modelFn, LLM: llm, Tools: []agent.Tool{
		tools.Clock{},
		tools.Weather{DefaultCity: func() string { return settings().City }},
		tools.Web{LLM: llm, Model: modelFn},
		tools.Playback{A: act, SetVolume: func(v int) error { return vol.Step(v - vol.Volume()) }},
		music(tools.PlayMusic{S: sp}),
		music(tools.LikeTrack{S: sp}),
		music(tools.NowPlaying{S: sp}),
	}}

	if err := os.MkdirAll(c.piperOut, 0o755); err != nil {
		log.Printf("piper: %v", err)
	}
	piper := &tts.Piper{Cmd: strings.Fields(c.piperCmd), OutDir: chrootPath(c.piperOut), HostOutDir: c.piperOut}
	go func() {
		if err := piper.Start(ctx); err != nil {
			log.Printf("piper: %v", err)
		}
	}()

	spk := newSpeaker(strings.Fields(c.speakerCmd), func() { out.refresh(vol.Volume()) })
	sess := assistant.New(assistant.Deps{
		Agent:  ag,
		TTS:    ttsChooser(settings, key, piper),
		Player: spk,
		Duck:   out.duck,
		LED:    led,
		Stock:  newStockSounds(c.sounds).Load,
	})
	log.Printf("assistant: on, model %s", model)
	return builtAssistant{sess, spk, link}
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
// behind it, or Piper alone.
func ttsChooser(settings func() daemon.AssistantSettings, key string, piper tts.Engine) func() tts.Engine {
	return func() tts.Engine {
		s := settings()
		if s.TTS != "gemini" {
			return piper
		}
		g := tts.NewGeminiTTS(key)
		if s.Voice != "" {
			g.Voice = s.Voice
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
	ok     atomic.Bool
	kick   chan struct{}
}

func newSpotifyLink(linked func() bool) *spotifyLink {
	return &spotifyLink{linked: linked, kick: make(chan struct{}, 1)}
}

// Linked is the last value read.
func (l *spotifyLink) Linked() bool { return l.ok.Load() }

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
		l.ok.Store(l.linked())
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
