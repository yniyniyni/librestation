package main

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"voice/internal/capture"
	"voice/internal/frontend"
	"voice/internal/kws"
	"voice/internal/wav"
)

// cmdReplay runs a 16-channel 48 kHz recording through the frontend and
// optionally scans the cleaned audio with every model.json under -models.
//
//	wakeword replay [-mics 3,4,5] [-gain 32] [-models dir] in16ch.wav [out16k.wav]
func cmdReplay(args []string) error {
	fl := flag.NewFlagSet("replay", flag.ExitOnError)
	mics := fl.String("mics", "3,4,5", "mic channels")
	gain := fl.Float64("gain", 32, "output gain")
	models := fl.String("models", "", "directory searched for model.json files")
	fl.Parse(args)
	if fl.NArg() < 1 || fl.NArg() > 2 {
		return fmt.Errorf("usage: wakeword replay [-mics 3,4,5] [-gain 32] [-models dir] in16ch.wav [out16k.wav]")
	}
	cfg := frontend.DefaultConfig()
	cfg.Gain = *gain
	cfg.MicChannels = nil
	for _, s := range strings.Split(*mics, ",") {
		c, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || c < 0 || c >= capture.Channels {
			return fmt.Errorf("bad mic channel %q", s)
		}
		cfg.MicChannels = append(cfg.MicChannels, c)
	}
	b, err := os.ReadFile(fl.Arg(0))
	if err != nil {
		return err
	}
	pcm, err := decodeMulti(b, capture.Channels, capture.Rate)
	if err != nil {
		return fmt.Errorf("%s: %w", fl.Arg(0), err)
	}
	f := frontend.New(cfg)
	const bs = capture.BlockFrames * capture.Channels
	var out []int16
	for p := 0; p+bs <= len(pcm); p += bs {
		out = append(out, f.Process(pcm[p:p+bs])...)
	}
	fmt.Fprintf(os.Stderr, "%.2fs in, %.2fs out\n", float64(len(pcm)/capture.Channels)/capture.Rate, float64(len(out))/kws.SampleRate)
	if fl.NArg() == 2 {
		if err := wav.WriteFile(fl.Arg(1), out, kws.SampleRate); err != nil {
			return err
		}
	}
	if *models == "" {
		return nil
	}
	var ms []*kws.Model
	filepath.WalkDir(*models, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "model.json" {
			if m, e := loadModel(p); e != nil {
				fmt.Fprintln(os.Stderr, "warning:", e)
			} else {
				ms = append(ms, m)
			}
		}
		return nil
	})
	if len(ms) == 0 {
		fmt.Fprintf(os.Stderr, "warning: no model.json found under %q\n", *models)
		return nil
	}
	det, err := kws.NewDetector(ms, make([]float32, len(ms)))
	if err != nil {
		return err
	}
	for pos := 0; pos < len(out); {
		n := min(kws.StepSamp, len(out)-pos)
		pos += n
		det.Push(out[pos-n:pos], func(i int, s float32) {
			fmt.Printf("t=%.2fs %s %.3f\n", float64(pos)/kws.SampleRate, ms[i].Name, s)
		})
	}
	return nil
}

// decodeMulti returns interleaved s16 samples of a WAV with exactly nch
// channels at the given rate.
func decodeMulti(b []byte, nch, rate int) ([]int16, error) {
	if len(b) < 12 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, errors.New("not a RIFF/WAVE file")
	}
	gotCh, gotRate := 0, 0
	for p := 12; p+8 <= len(b); {
		id, size := string(b[p:p+4]), int(binary.LittleEndian.Uint32(b[p+4:]))
		body := b[p+8:]
		size = min(size, len(body))
		switch id {
		case "fmt ":
			if size < 16 || binary.LittleEndian.Uint16(body[14:]) != 16 {
				return nil, errors.New("need 16-bit PCM")
			}
			gotCh = int(binary.LittleEndian.Uint16(body[2:]))
			gotRate = int(binary.LittleEndian.Uint32(body[4:]))
		case "data":
			if gotCh != nch || gotRate != rate {
				return nil, fmt.Errorf("got %d ch @ %d Hz, need %d ch @ %d Hz", gotCh, gotRate, nch, rate)
			}
			pcm := make([]int16, size/2)
			for i := range pcm {
				pcm[i] = int16(binary.LittleEndian.Uint16(body[2*i:]))
			}
			return pcm, nil
		}
		p += 8 + size + size%2
	}
	return nil, errors.New("no data chunk")
}
