package tools

import (
	"context"
	"fmt"
	"time"

	"voice/internal/agent"
)

// Actuator controls the player; daemon.Actuator has the same method set.
type Actuator interface {
	Pause() error
	Play() error
	Next() error
	Prev() error
	Step(delta int) error
}

// volumeStep is the change of louder/quieter; it matches daemon.VolumeStep.
const volumeStep = 5

// Playback controls music playback and volume.
type Playback struct {
	A         Actuator
	SetVolume func(int) error // the unified volume
	Shuffle   func(ctx context.Context, on bool) error
	// Seek moves playback to a position, or by it when relative.
	Seek func(position time.Duration, relative bool) error
}

func (Playback) Decl() agent.FuncDecl {
	return agent.FuncDecl{
		Name: "playback",
		Description: "Control music playback and volume: pause, play, next or previous track, louder, quieter, set the volume to a value, shuffle what is playing or stop shuffling, move within the track. " +
			"seek_by moves by value seconds, negative for back: \"перемотай на 30 секунд вперёд\" is 30, \"назад на минуту\" is -60. " +
			"seek_to goes to value seconds from the track's start: 0 for its start. " +
			"Use shuffle_on for any request to shuffle what plays now, without asking what to play. " +
			"Smart Shuffle can only be turned on in the Spotify app: if asked for it, use shuffle_on and say so. " +
			"play continues whatever played last: use it for \"turn on the music\" with no title, artist or genre; " +
			"to start something specific use play_music instead.",
		Params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type": "string",
					"enum": []string{"pause", "play", "next", "prev", "louder", "quieter", "set_volume", "shuffle_on", "shuffle_off", "seek_by", "seek_to"},
				},
				"value": map[string]any{"type": "integer", "description": "Volume 0..100 for set_volume; seconds for seek_by and seek_to"},
			},
			"required": []string{"action"},
		},
	}
}

func (p Playback) Run(ctx context.Context, args map[string]any) (map[string]any, error) {
	action, _ := args["action"].(string)
	var err error
	switch action {
	case "pause":
		err = p.A.Pause()
	case "play":
		err = p.A.Play()
	case "next":
		err = p.A.Next()
	case "prev":
		err = p.A.Prev()
	case "louder":
		err = p.A.Step(volumeStep)
	case "quieter":
		err = p.A.Step(-volumeStep)
	case "shuffle_on", "shuffle_off":
		if p.Shuffle == nil {
			return nil, fmt.Errorf("перемешивание недоступно")
		}
		err = p.Shuffle(ctx, action == "shuffle_on")
	case "seek_by", "seek_to":
		v, ok := args["value"].(float64)
		if !ok || (action == "seek_to" && v < 0) {
			return nil, fmt.Errorf("перемотка требует value в секундах")
		}
		if p.Seek == nil {
			return nil, fmt.Errorf("перемотка недоступна")
		}
		err = p.Seek(time.Duration(v*float64(time.Second)), action == "seek_by")
	case "set_volume":
		v, ok := args["value"].(float64)
		if !ok || v < 0 || v > 100 {
			return nil, fmt.Errorf("set_volume требует value от 0 до 100")
		}
		err = p.SetVolume(int(v))
	default:
		return nil, fmt.Errorf("неизвестное действие %q", action)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}
