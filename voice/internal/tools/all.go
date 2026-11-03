package tools

import (
	"context"
	"time"

	"voice/internal/agent"
	"voice/internal/alarms"
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
	// Radio plays a track followed by its station; Shuffle turns shuffle on or off.
	Radio   func(ctx context.Context, track string) error
	Shuffle func(ctx context.Context, on bool) error
	Seek    func(position time.Duration, relative bool) error
	Alarms  *alarms.Manager
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
		Playback{A: d.Actuator, SetVolume: d.SetVolume, Shuffle: d.Shuffle, Seek: d.Seek},
		music(PlayMusic{S: d.Spotify, Radio: d.Radio, Shuffle: d.Shuffle}),
		music(LikeTrack{S: d.Spotify}),
		music(NowPlaying{S: d.Spotify}),
		Alarm{M: d.Alarms, Find: func(ctx context.Context, query, kind string) (alarms.Music, error) {
			return FindMusic(ctx, d.Spotify, query, kind)
		}},
		TimerTool{M: d.Alarms},
		AlarmsList{M: d.Alarms},
	}
}
