package main

import (
	"context"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"voice/internal/volume"
)

// output turns the one volume into loudness: the gain file for aaudio_pipe is
// written at once, the amplifier (a slow app_process helper, ~1 s) is set in
// the background to the latest volume only.
type output struct {
	hwCmd    []string
	gainFile string
	hw       chan int
}

func newOutput(hwCmd []string, gainFile string) *output {
	return &output{hwCmd: hwCmd, gainFile: gainFile, hw: make(chan int, 1)}
}

// set is volume.Sync.Out: it must not block.
func (o *output) set(v int) {
	if o.gainFile != "" {
		g := strconv.FormatFloat(volume.Compensation(v), 'g', 8, 64)
		tmp := o.gainFile + ".tmp"
		err := os.WriteFile(tmp, []byte(g+"\n"), 0o644)
		if err == nil {
			err = os.Rename(tmp, o.gainFile)
		}
		if err != nil {
			log.Printf("gain: %v", err)
		}
	}
	if len(o.hwCmd) == 0 {
		return
	}
	select { // keep only the newest value
	case <-o.hw:
	default:
	}
	o.hw <- v
}

func (o *output) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case v := <-o.hw:
			args := append(o.hwCmd[1:len(o.hwCmd):len(o.hwCmd)], strconv.Itoa(v))
			out, err := exec.CommandContext(ctx, o.hwCmd[0], args...).CombinedOutput()
			if err != nil {
				log.Printf("amplifier volume %d: %v: %s", v, err, strings.TrimSpace(string(out)))
			} else {
				log.Printf("amplifier volume: %s", strings.TrimSpace(string(out)))
			}
		}
	}
}
