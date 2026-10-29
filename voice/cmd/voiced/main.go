// Command voiced runs user-trained voice commands on a Yandex Station.
package main

import (
	"fmt"
	"os"
	"runtime/debug"
	_ "time/tzdata" // Android has no zoneinfo; embed it
)

const usage = `usage:
  voiced run [flags]        station daemon: detect words, web UI, training
  voiced train [flags]      train a word from WAV files
  voiced scan model.json... audio.wav
  voiced replay [flags] in16ch.wav [out16k.wav]
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	// Minis have ~120 MB free next to maind; training holds ~45 MB live, so
	// keep the GC from letting the heap double on top of that.
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(64 << 20)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(os.Args[2:])
	case "train":
		err = cmdTrain(os.Args[2:])
	case "replay":
		err = cmdReplay(os.Args[2:])
	case "scan":
		err = cmdScan(os.Args[2:])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "voiced:", err)
		os.Exit(1)
	}
}
