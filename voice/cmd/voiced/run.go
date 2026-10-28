package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"voice/internal/daemon"
	"voice/internal/input"
	"voice/internal/soloist"
	"voice/internal/volume"
)

func cmdRun(args []string) error {
	exe, _ := os.Executable()
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var opt daemon.Options
	var micCmd, hwCmd, knob string
	fs.StringVar(&opt.DataDir, "data", "/data/local/tmp/voice/data", "words and settings")
	fs.StringVar(&micCmd, "mic-cmd", "sh /data/local/tmp/voice/mic.sh", "mic capture helper command")
	fs.StringVar(&opt.Soloist, "soloist", "ws://127.0.0.1:9771", "Soloist WebSocket URL")
	fs.StringVar(&opt.Negatives, "negatives", filepath.Join(filepath.Dir(exe), "negatives.bin"), "bundled negative set")
	fs.StringVar(&opt.Listen, "listen", ":8780", "web UI address")
	fs.StringVar(&hwCmd, "hw-cmd", "sh /data/local/tmp/voice/hwvol.sh 14", "pins the hardware volume once at start; empty = leave it")
	fs.StringVar(&knob, "knob", "/dev/input/event2", "volume knob evdev device; empty = none")
	fs.Parse(args)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	opt.MicCmd = strings.Fields(micCmd)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Soloist's volume is the master (see package volume); the hardware level
	// is pinned once and never touched again.
	go pinHardware(strings.Fields(hwCmd))
	var vol *volume.Sync
	volReady := make(chan struct{})
	sol := soloist.Dial(ctx, opt.Soloist, func(e soloist.Event) {
		select {
		case <-volReady:
			vol.OnEvent(e)
		default:
		}
	})
	defer sol.Close()
	vol = volume.New(sol, filepath.Join(opt.DataDir, "volume"))
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

	srv := &http.Server{Addr: opt.Listen, Handler: d.Handler(), ReadHeaderTimeout: 10 * time.Second}
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

// pinHardware runs the hardware-volume helper once.
func pinHardware(cmd []string) {
	if len(cmd) == 0 {
		return
	}
	out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
	if err != nil {
		log.Printf("hardware volume: %v: %s", err, strings.TrimSpace(string(out)))
		return
	}
	log.Printf("hardware volume pinned: %s", strings.TrimSpace(string(out)))
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
