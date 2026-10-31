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
	"sync/atomic"
	"syscall"
	"time"

	"voice/internal/daemon"
	"voice/internal/input"
	"voice/internal/judge"
	"voice/internal/led/proto"
	"voice/internal/soloist"
	"voice/internal/volume"
)

func cmdRun(args []string) error {
	exe, _ := os.Executable()
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var opt daemon.Options
	var micCmd, hwCmd, gainFile, knob, ledSock, leddURL, judgeKey, judgeModel string
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
	fs.Parse(args)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	opt.MicCmd = strings.Fields(micCmd)
	useAndroidNet()
	if key, err := judge.ReadKey(judgeKey); err != nil {
		log.Printf("judge: %v; hits are not judged", err)
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
	if ledSock != "" {
		sender := proto.NewSender(ledSock)
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
