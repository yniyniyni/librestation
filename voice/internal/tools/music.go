package tools

import (
	"context"
	"fmt"
	"strings"

	"voice/internal/agent"
	"voice/internal/spotifyapi"
)

// PlayMusic plays something from Spotify on the speaker.
type PlayMusic struct{ S *spotifyapi.Client }

func (PlayMusic) Decl() agent.FuncDecl {
	return agent.FuncDecl{
		Name:        "play_music",
		Description: "Find and play music on the speaker through Spotify: an artist, a song, an album or a playlist.",
		Params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "What to play, e.g. an artist or a song name"},
				"kind":  map[string]any{"type": "string", "enum": []string{"track", "artist", "album", "playlist", "any"}, "description": "What the query names; any if unsure"},
			},
			"required": []string{"query"},
		},
	}
}

func (p PlayMusic) Run(ctx context.Context, args map[string]any) (map[string]any, error) {
	q, _ := args["query"].(string)
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, fmt.Errorf("не знаю, что включить")
	}
	kind, _ := args["kind"].(string)
	if kind == "" {
		kind = "any"
	}
	it, err := p.S.Search(ctx, q, kind)
	if err != nil {
		return nil, err
	}
	if err := p.S.Play(ctx, it); err != nil {
		return nil, err
	}
	return map[string]any{"playing": it.Name}, nil
}

// LikeTrack saves the playing track to the library or removes it.
type LikeTrack struct{ S *spotifyapi.Client }

func (LikeTrack) Decl() agent.FuncDecl {
	return agent.FuncDecl{
		Name:        "like_track",
		Description: "Add the track that is playing now to the user's Spotify favourites, or remove it.",
		Params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"liked": map[string]any{"type": "boolean", "description": "true to like, false to remove the like; default true"},
			},
		},
	}
}

func (l LikeTrack) Run(ctx context.Context, args map[string]any) (map[string]any, error) {
	liked := true
	if v, ok := args["liked"].(bool); ok {
		liked = v
	}
	it, ok, err := l.S.Current(ctx)
	if err != nil {
		return nil, err
	}
	if !ok || it.Kind != "track" {
		return nil, fmt.Errorf("сейчас ничего не играет")
	}
	if err := l.S.SetLiked(ctx, it.URI, liked); err != nil {
		return nil, err
	}
	return map[string]any{"track": it.Name, "liked": liked}, nil
}

// NowPlaying names the current track.
type NowPlaying struct{ S *spotifyapi.Client }

func (NowPlaying) Decl() agent.FuncDecl {
	return agent.FuncDecl{
		Name:        "now_playing",
		Description: "Tell which track is playing now and who performs it.",
		Params:      map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

func (n NowPlaying) Run(ctx context.Context, _ map[string]any) (map[string]any, error) {
	it, ok, err := n.S.Current(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("сейчас ничего не играет")
	}
	return map[string]any{"track": it.Name, "artist": it.Artist}, nil
}
