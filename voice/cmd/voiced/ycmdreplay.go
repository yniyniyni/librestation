package main

import (
	"flag"
	"fmt"
	"os"

	"voice/internal/capture"
	"voice/internal/ycmd"
)

// cmdYcmd runs 16-channel 48 kHz recordings through the Yandex command
// spotter, one fresh spotter per file, and prints every fire.
//
//	voiced ycmd [-dir model] in16ch.wav...
func cmdYcmd(args []string) error {
	fl := flag.NewFlagSet("ycmd", flag.ExitOnError)
	dir := fl.String("dir", "", "directory with acoustic_model.nnet, flags.txt, words.txt")
	fl.Parse(args)
	if fl.NArg() < 1 || *dir == "" {
		return fmt.Errorf("usage: voiced ycmd -dir model in16ch.wav...")
	}
	m, err := ycmd.Load(*dir)
	if err != nil {
		return err
	}
	for _, path := range fl.Args() {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		pcm, err := decodeMulti(b, capture.Channels, capture.Rate)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		sp, in := ycmd.NewSpotter(m), ycmd.NewInput()
		const bs = capture.BlockFrames * capture.Channels
		fires, done := 0, 0
		for p := 0; p+bs <= len(pcm); p += bs {
			x := in.Process(pcm[p : p+bs])
			done += len(x)
			sp.Push(x, func(cmd string, conf float64) {
				fires++
				fmt.Printf("%s %.2f %s %.3f\n", path, float64(done)/16000, cmd, conf)
			})
		}
		fmt.Printf("%s: %d fires in %.2fs\n", path, fires, float64(done)/16000)
	}
	return nil
}
