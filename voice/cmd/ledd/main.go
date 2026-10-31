// Command ledd owns the LED ring and screen of the speaker: it renders the
// scene from socket events, Soloist playback state and the light sensor.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // Android has no zoneinfo; embed it

	"voice/internal/led/asset"
	"voice/internal/led/hw"
	"voice/internal/led/proto"
	"voice/internal/led/scene"
	"voice/internal/soloist"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Fatalf("ledd: %v", err)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("ledd", flag.ExitOnError)
	var assets, data, sock, listen, solURL string
	var fps int
	fs.StringVar(&assets, "assets", "/data/local/tmp/voice/assets", "stock assets directory")
	fs.StringVar(&data, "data", "/data/local/tmp/voice/data", "settings directory")
	fs.StringVar(&sock, "sock", "/data/local/tmp/voice/ledd.sock", "event socket")
	fs.StringVar(&listen, "listen", "127.0.0.1:8781", "HTTP API address")
	fs.StringVar(&solURL, "soloist", "ws://127.0.0.1:9771", "Soloist WebSocket URL")
	fs.IntVar(&fps, "fps", 25, "render frames per second")
	fs.Parse(args)
	if fps < 1 || fps > 100 {
		return fmt.Errorf("-fps %d out of range 1..100", fps)
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	setLocalZone()
	// A broken framebuffer must not leave the screen at full brightness.
	safeBacklight(hw.DevicePaths())

	w, err := hw.Open(hw.DevicePaths(), 0, true)
	if err != nil {
		return err
	}
	defer w.Close()

	sc := scene.New(loadAssets(assets), time.Now)
	a := newAPI(data, sc, realAfter)

	lst, err := proto.Listen(sock, func(e proto.Event) { handleEvent(sc, a, e) })
	if err != nil {
		return err
	}
	defer lst.Close()

	pb := &playback{sc: sc}
	sol := soloist.Dial(ctx, solURL, pb.onEvent)
	defer sol.Close()
	go watchSoloist(ctx, sol, pb)

	srv := &http.Server{Addr: listen, Handler: a.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	go render(ctx, w, sc, fps)
	log.Printf("ledd: API on %s, socket %s, data in %s", listen, sock, data)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func handleEvent(sc *scene.Scene, a *api, e proto.Event) {
	switch e.T {
	case "volume":
		sc.Volume(e.V, e.Dir)
	case "cmd":
		sc.Command(e.A)
	case "audio":
		sc.Audio(e.Level, bands12(e.Bands))
	case "ai":
		a.setAI(e.On, e.A, 0)
	}
}

// loadAssets reads the optional stock animations; missing files are skipped.
func loadAssets(dir string) scene.Assets {
	as := scene.Assets{
		Pulse: map[string][11]*asset.Anim{},
		Icons: map[string]*asset.Anim{},
	}
	n := 0
	if f, err := asset.LoadClockFont(filepath.Join(dir, "fonts", "clock_large.gif")); err == nil {
		as.Font = f
		n++
	}
	icons := map[string]string{"pause": "Pause", "play": "Resume", "next": "Next", "prev": "Previous"}
	for key, name := range icons {
		if an, err := asset.LoadGIF(filepath.Join(dir, "25x16", "media", "25x16_"+name+".gif")); err == nil {
			as.Icons[key] = an
			n++
		}
	}
	for _, dir2 := range []string{"up", "down"} {
		var set [11]*asset.Anim
		for i := range set {
			p := filepath.Join(dir, "25x16", "rcu_volume", "volume_pulse", dir2, fmt.Sprintf("%d.gif", i))
			if an, err := asset.LoadGIF(p); err == nil {
				set[i] = an
				n++
			}
		}
		as.Pulse[dir2] = set
	}
	log.Printf("ledd: loaded %d assets from %s", n, dir)
	return as
}

// playback turns Soloist statuses into scene.Playing.
type playback struct{ sc *scene.Scene }

func (p *playback) onEvent(e soloist.Event) {
	if (e.Type != "playback_state" && e.Type != "playback_changed") || e.Status == "" {
		return
	}
	switch e.Status {
	case "playing":
		p.sc.Playing(true)
	case "buffering": // track changes and seeks: keep the previous state
	default:
		p.sc.Playing(false)
	}
}

// playerScene is the part of the scene the Soloist watcher drives.
type playerScene interface{ Playing(bool) }

// soloistSync decides, once per tick, when to ask Soloist for its state: after
// every (re)connect, retried each tick until the request goes out.
type soloistSync struct {
	sc      playerScene
	send    func() error
	lastGen uint64
	pending bool
}

func (s *soloistSync) step(connected bool, gen uint64) {
	if gen != s.lastGen {
		s.lastGen = gen
		s.pending = true // reconnected, possibly between two ticks
	}
	if !connected {
		s.sc.Playing(false)
		return
	}
	if s.pending && s.send() == nil {
		s.pending = false
	}
}

// watchSoloist marks playback stopped while Soloist is unreachable and asks
// for the current state after every (re)connect.
func watchSoloist(ctx context.Context, sol *soloist.Client, pb *playback) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	s := &soloistSync{sc: pb.sc, send: func() error { return sol.Command("get_state") }}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.step(sol.Connected(), sol.Generation())
	}
}

// safeBacklight sets a dark-safe backlight: from the light sensor when it is
// readable, otherwise the darkest level.
func safeBacklight(p hw.Paths) {
	lux := 0.0
	if b, err := os.ReadFile(p.Lux); err == nil {
		if v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64); err == nil {
			lux = v
		}
	}
	bl, _ := hw.Brightness(lux)
	if err := hw.SetBacklightPath(p.Backlight, bl); err != nil {
		log.Printf("ledd: initial backlight: %v", err)
	}
}

// setLocalZone fixes time.Local on Android, where there is no /etc/localtime
// and TZ is usually unset: ask the system property instead.
func setLocalZone() {
	if os.Getenv("TZ") != "" {
		return
	}
	out, err := exec.Command("getprop", "persist.sys.timezone").Output()
	name := strings.TrimSpace(string(out))
	if err != nil || name == "" {
		return
	}
	if loc, err := time.LoadLocation(name); err == nil {
		time.Local = loc
	}
}

// rateLog logs at most once per interval.
type rateLog struct {
	mu    sync.Mutex
	last  time.Time
	every time.Duration
}

func (r *rateLog) printf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.last.IsZero() && time.Since(r.last) < r.every {
		return
	}
	r.last = time.Now()
	log.Printf(format, args...)
}

func render(ctx context.Context, w *hw.Writer, sc *scene.Scene, fps int) {
	errLog := &rateLog{every: time.Minute}
	luxLog := &rateLog{every: time.Minute}
	ringScale := 1.0
	level := sc.Brightness() // 0 = automatic, 1..100 = fixed by the user
	updateBrightness := func() {
		lux, err := w.ReadLux()
		if err != nil {
			errLog.printf("ledd: lux: %v", err)
			lux = 0
		} else {
			luxLog.printf("ledd: lux %.1f", lux)
		}
		bl, rs := hw.Brightness(lux)
		if level > 0 {
			bl, rs = hw.FixedBrightness(level)
		}
		ringScale = rs
		if err := w.SetBacklight(bl); err != nil {
			errLog.printf("ledd: backlight: %v", err)
		}
	}
	updateBrightness()

	frame := time.NewTicker(time.Second / time.Duration(fps))
	defer frame.Stop()
	bright := time.NewTicker(2 * time.Second)
	defer bright.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-bright.C:
			updateBrightness()
		case <-frame.C:
			if l := sc.Brightness(); l != level { // apply a new setting at once
				level = l
				updateBrightness()
			}
			s, r := sc.Frame()
			if err := w.Draw(s, r, ringScale); err != nil {
				errLog.printf("ledd: draw: %v", err)
			}
		}
	}
}
