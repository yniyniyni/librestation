package tools

import (
	"context"

	"voice/internal/agent"
	"voice/internal/spotifyapi"
)

// Deps are what the assistant's tools need; the zero value is enough to list
// them (Decl only).
type Deps struct {
	Spotify     *spotifyapi.Client
	Actuator    Actuator
	SetVolume   func(int) error
	DefaultCity func() string
	LLM         interface {
		Stream(ctx context.Context, r agent.Request, onText func(string)) (agent.Content, error)
	}
	Model func() string
	// Music wraps each Spotify tool (nil = as is).
	Music func(agent.Tool) agent.Tool
}

// All is the one list of the assistant's tools.
func All(d Deps) []agent.Tool {
	music := d.Music
	if music == nil {
		music = func(t agent.Tool) agent.Tool { return t }
	}
	return []agent.Tool{
		Clock{},
		Weather{DefaultCity: d.DefaultCity},
		Web{LLM: d.LLM, Model: d.Model},
		Playback{A: d.Actuator, SetVolume: d.SetVolume},
		music(PlayMusic{S: d.Spotify}),
		music(LikeTrack{S: d.Spotify}),
		music(NowPlaying{S: d.Spotify}),
	}
}
