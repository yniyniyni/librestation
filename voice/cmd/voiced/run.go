package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"voice/internal/daemon"
)

func cmdRun(args []string) error {
	exe, _ := os.Executable()
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var opt daemon.Options
	var micCmd string
	fs.StringVar(&opt.DataDir, "data", "/data/local/tmp/voice/data", "words and settings")
	fs.StringVar(&micCmd, "mic-cmd", "sh /data/local/tmp/voice/mic.sh", "mic capture helper command")
	fs.StringVar(&opt.Soloist, "soloist", "ws://127.0.0.1:9771", "Soloist WebSocket URL")
	fs.StringVar(&opt.Negatives, "negatives", filepath.Join(filepath.Dir(exe), "negatives.bin"), "bundled negative set")
	fs.StringVar(&opt.Listen, "listen", ":8780", "web UI address")
	fs.Parse(args)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	opt.MicCmd = strings.Fields(micCmd)
	opt.Actuator = logActuator{}

	d, err := daemon.New(opt)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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

// logActuator stands in until the Soloist actuator is wired: it only logs.
type logActuator struct{}

func (logActuator) Pause() error { log.Print("action pause"); return nil }
func (logActuator) Play() error  { log.Print("action play"); return nil }
func (logActuator) Next() error  { log.Print("action next"); return nil }
func (logActuator) Prev() error  { log.Print("action prev"); return nil }
func (logActuator) Step(delta int) error {
	log.Printf("action step %+d", delta)
	return nil
}
