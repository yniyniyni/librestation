// Package tts turns reply text into speech: Piper (a resident process on the
// speaker) and Gemini TTS (a cloud engine), with a fallback between them.
package tts

import (
	"context"
	"log"
)

// Engine synthesises text into mono PCM16 at the rate it reports.
type Engine interface {
	Synth(ctx context.Context, text string) (pcm []int16, rate int, err error)
}

// Fallback uses Backup when Primary fails (Gemini → Piper).
type Fallback struct{ Primary, Backup Engine }

func (f Fallback) Synth(ctx context.Context, text string) ([]int16, int, error) {
	pcm, rate, err := f.Primary.Synth(ctx, text)
	if err == nil {
		return pcm, rate, nil
	}
	if ctx.Err() != nil || f.Backup == nil {
		return nil, 0, err
	}
	log.Printf("tts: primary failed, using backup: %v", err)
	return f.Backup.Synth(ctx, text)
}
