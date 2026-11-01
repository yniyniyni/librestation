package tools

import (
	"context"
	"fmt"

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
}

func (Playback) Decl() agent.FuncDecl {
	return agent.FuncDecl{
		Name: "playback",
		Description: "Control music playback and volume: pause, play, next or previous track, louder, quieter, or set the volume to a value. " +
			"play continues whatever played last: use it for \"turn on the music\" with no title, artist or genre; " +
			"to start something specific use play_music instead.",
		Params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{
					"type": "string",
					"enum": []string{"pause", "play", "next", "prev", "louder", "quieter", "set_volume"},
				},
				"value": map[string]any{"type": "integer", "description": "Volume 0..100, for set_volume"},
			},
			"required": []string{"action"},
		},
	}
}

func (p Playback) Run(_ context.Context, args map[string]any) (map[string]any, error) {
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
