package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	iofs "io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"voice/internal/alarms"
	"voice/internal/daemon"
	"voice/internal/input"
	"voice/internal/judge"
	"voice/internal/led/proto"
	"voice/internal/mdns"
	"voice/internal/soloist"
	"voice/internal/spotifyapi"
	"voice/internal/tools"
	"voice/internal/volume"
	"voice/internal/ycmd"
)

func cmdRun(args []string) error {
	exe, _ := os.Executable()
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var opt daemon.Options
	var micCmd, hwCmd, gainFile, knob, muteSw, ledSock, leddURL, judgeKey, judgeModel string
	var ac assistantConfig
	var ycmdDir, wakeDir string
	fs.StringVar(&opt.DataDir, "data", "/data/local/tmp/voice/data", "words and settings")
	fs.StringVar(&micCmd, "mic-cmd", "sh /data/local/tmp/voice/mic.sh", "mic capture helper command")
	fs.StringVar(&opt.Soloist, "soloist", "ws://127.0.0.1:9771", "Soloist WebSocket URL")
	fs.StringVar(&opt.Negatives, "negatives", filepath.Join(filepath.Dir(exe), "negatives.bin"), "bundled negative set")
	fs.StringVar(&opt.Listen, "listen", ":8780", "web UI address")
	fs.StringVar(&hwCmd, "hw-cmd", "sh /data/local/tmp/voice/hwvol.sh -", "amplifier helper reading 0..100 per stdin line; empty = leave it")
	fs.StringVar(&gainFile, "gain-file", "/data/local/tmp/soloist/gain", "aaudio_pipe gain that undoes Soloist's software volume; empty = none")
	fs.StringVar(&knob, "knob", "/dev/input/event2", "volume knob evdev device; empty = none")
	fs.StringVar(&muteSw, "mute-switch", "/dev/input/event0", "mic button evdev device; empty = none")
	fs.StringVar(&ledSock, "led-sock", "/data/local/tmp/voice/ledd.sock", "ledd event socket; empty = no LED events")
	fs.StringVar(&leddURL, "ledd", "http://127.0.0.1:8781", "ledd HTTP API behind /api/led")
	fs.StringVar(&judgeKey, "judge-key", "/data/local/tmp/voice/gemini.key", "Gemini API key file; missing = hits are not judged")
	fs.StringVar(&judgeModel, "judge-model", judge.DefaultModel, "Gemini model that judges hits")
	fs.StringVar(&ac.spotifyToken, "spotify-token", "/data/local/tmp/voice/spotify.json", "Spotify refresh token file written by setup")
	fs.StringVar(&ac.spotifyDevice, "spotify-device", "", "this speaker's Spotify Connect name; empty = the active device")
	fs.StringVar(&ac.speakerCmd, "speaker-cmd", "/data/local/tmp/soloist/aaudio_pipe 24000", "helper that plays replies (float32 stereo on stdin)")
	fs.StringVar(&ac.liveModel, "live-model", "gemini-3.8-live", "Gemini Live audio model")
	fs.StringVar(&ac.piperCmd, "piper-cmd", "sh /data/local/tmp/soloist/chroot.sh /opt/piper/piper --model /opt/piper/voice.onnx --json-input", "resident Piper command")
	fs.StringVar(&ac.piperOut, "piper-out", "/data/local/tmp/soloist/rootfs/tmp/tts", "where Piper writes, as voiced sees it (inside the chroot's rootfs)")
	fs.StringVar(&ac.sounds, "sounds", "/data/local/tmp/voice/sounds", "stock phrases <name>.wav")
	fs.StringVar(&wakeDir, "wake-dir", "/data/local/tmp/voice/yandex/activation", "Yandex's stock trigger words extracted from the firmware, one directory each")
	fs.StringVar(&ycmdDir, "ycmd-dir", "/data/local/tmp/voice/yandex/command", "Yandex quick-command model extracted from the firmware by setup")
	fs.Parse(args)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	opt.MicCmd = strings.Fields(micCmd)
	if muteSw != "" {
		opt.MicMuted = micSwitch(muteSw)
	}
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

	// A missing or broken model only turns quick commands off.
	if m, err := ycmd.Load(ycmdDir); err != nil {
		opt.CommandsErr = "не удалось загрузить модель: " + err.Error()
		if errors.Is(err, iofs.ErrNotExist) {
			opt.CommandsErr = "модель не найдена — запусти ./setup"
		}
		log.Printf("ycmd: %v; quick commands are off", err)
	} else {
		opt.Commands = m
		log.Printf("ycmd: model %s", m.Version)
		if m.Version != ycmd.TestedVersion {
			log.Printf("ycmd: warning: model %q is not the tested %q", m.Version, ycmd.TestedVersion)
		}
		if miss := daemon.MissingStockCommands(m); len(miss) > 0 {
			log.Printf("ycmd: в модели нет команд %s — они не сработают", strings.Join(miss, ", "))
		}
	}

	opt.Wake, opt.WakeErr = loadStockWake(wakeDir)

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
	if hwCmd != "" {
		if m, err := openMixerControl(mixerDevice, ampControl); err != nil {
			log.Printf("amplifier watch: %v", err)
		} else {
			go out.watchAmp(ctx, m.read)
		}
	}
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
	opt.Actuator = actuator{sol, vol}

	// The speaker and Spotify serve alarms even with no assistant to talk to.
	spk := newSpeaker(strings.Fields(ac.speakerCmd), func() { out.refresh(vol.Volume()) })
	defer spk.Close()
	sp := spotifyapi.New(ac.spotifyToken, ac.spotifyDevice)
	rng := &ringer{
		sounds: filepath.Join(ac.sounds, "alarm"),
		play:   spk.Play,
		flush:  spk.Flush,
		hold:   out.hold,
		volume: vol.Volume,
		playing: func() bool {
			d := dmn.Load()
			return d != nil && d.Playing()
		},
		pause:  sol.Pause,
		resume: sol.Play,
		music: func(ctx context.Context, query string) error {
			_, err := tools.PlayMusic{S: sp, Radio: sol.PlayRadio, Shuffle: sol.SetShuffle}.
				Run(ctx, map[string]any{"query": query, "kind": "any"})
			return err
		},
		led:   ledSend,
		now:   time.Now,
		sleep: sleepCtx,
		ticks: newTicks,
	}
	alm := alarms.New(filepath.Join(opt.DataDir, "alarms.json"), rng, time.Now)
	opt.Ring = alm
	if knob != "" {
		go readKnob(ctx, knob, func(code uint16) {
			if alm.ConsumesKnob() {
				alm.Stop()
				return
			}
			vol.Key(code)
		})
	}

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
		model := func() string {
			if d := dmn.Load(); d != nil {
				return d.AssistantModel()
			}
			return judgeModel
		}
		wakeWords := func() []string {
			if d := dmn.Load(); d != nil {
				return d.WakeWords()
			}
			return nil
		}
		a := buildAssistant(ctx, ac, key, judgeModel, model, settings, wakeWords, opt.Actuator, sol, sp, spk, alm, vol, out, ledSend)
		rng.hush = a.session.Hush
		opt.Assistant = a.session
		opt.LiveModel = ac.liveModel
		opt.AssistantStatus = func() any { return a.session.Status() }
		opt.SpotifyLinked = a.link.Linked
		opt.PiperMissing = a.piperDown.Load
	}

	led, err := ledProxy(leddURL)
	if err != nil {
		return fmt.Errorf("-ledd: %w", err)
	}
	opt.LED, opt.Volume = led, vol

	d, err := daemon.New(opt)
	if err != nil {
		return err
	}
	dmn.Store(d)
	go alm.Run(ctx)

	srv := &http.Server{Addr: opt.Listen, Handler: d.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	go d.Run(ctx)
	if _, port, err := net.SplitHostPort(opt.Listen); err == nil {
		if n, err := strconv.Atoi(port); err == nil {
			go mdns.Serve(ctx, mdns.Service{Name: "_librestation._tcp", Port: n,
				TXT: []string{fmt.Sprintf("api=%d", daemon.APIVersion), "path=/api/v1"}})
		}
	}
	log.Printf("voiced: UI on %s, data in %s", opt.Listen, opt.DataDir)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// stockWakeWords are the firmware's trigger words, in the panel's order.
var stockWakeWords = []struct{ id, name string }{{"alisa", "Алиса"}, {"yandex", "Яндекс"}}

// loadStockWake loads the stock trigger words; a missing or broken one only
// leaves that word out.
func loadStockWake(dir string) ([]daemon.WakeModel, string) {
	var out []daemon.WakeModel
	msg := ""
	for _, w := range stockWakeWords {
		m, err := ycmd.Load(filepath.Join(dir, w.id))
		if err != nil {
			log.Printf("ycmd: stock word %s: %v", w.id, err)
			msg = "не удалось загрузить модель: " + err.Error()
			if errors.Is(err, iofs.ErrNotExist) {
				msg = "модели не найдены — запусти ./setup"
			}
			continue
		}
		log.Printf("ycmd: stock word %s, model %s", w.id, m.Version)
		out = append(out, daemon.WakeModel{ID: w.id, Name: w.name, Model: m})
	}
	return out, msg
}

// actuator sends transport commands to Soloist and volume steps to the
// unified volume.
type actuator struct {
	*soloist.Client
	vol *volume.Sync
}

var _ daemon.Actuator = actuator{}

func (a actuator) Step(delta int) error { return a.vol.Step(delta) }

// micSwitch returns a reader of the mic button. A failed read counts as not
// muted, which leaves capture to its silence restarts; it is logged once.
func micSwitch(path string) func() bool {
	var failed bool
	return func() bool {
		on, err := input.SwitchOn(path, input.SwMuteDevice)
		if err != nil && !failed {
			failed = true
			log.Printf("mic switch: %v", err)
		}
		return on
	}
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
