package tools

import (
	"context"
	"fmt"
	"log"
	"strings"

	"voice/internal/agent"
	"voice/internal/spotifyapi"
)

// PlayMusic plays something from Spotify on the speaker.
type PlayMusic struct{ S *spotifyapi.Client }

func (PlayMusic) Decl() agent.FuncDecl {
	return agent.FuncDecl{
		Name:        "play_music",
		Description: "Find and play music on the speaker through Spotify: an artist, a song, an album, a playlist, the user's Liked Songs or one of the user's own playlists.",
		Params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "What to play: a song with its artist, an artist, an album or a playlist's name, spelled as it is really written (foreign names in Latin letters). Not needed for liked"},
				"kind": map[string]any{"type": "string", "enum": []string{"track", "artist", "album", "playlist", "any", "liked", "my_playlist"},
					"description": "What the query names; any if unsure. liked: the user's Liked Songs (favourites). my_playlist: one of the user's own playlists; if there is none by that name the error lists them, choose from it"},
				"shuffle": map[string]any{"type": "boolean", "description": "Shuffle, when asked to"},
			},
		},
	}
}

func (p PlayMusic) Run(ctx context.Context, args map[string]any) (map[string]any, error) {
	q, _ := args["query"].(string)
	q = strings.TrimSpace(q)
	kind, _ := args["kind"].(string)
	if kind == "" {
		kind = "any"
	}
	if q == "" && kind != "liked" {
		return nil, fmt.Errorf("не знаю, что включить")
	}
	var it spotifyapi.Item
	var err error
	switch kind {
	case "liked":
		it = spotifyapi.Liked()
	case "my_playlist":
		it, err = p.S.MyPlaylist(ctx, q)
	default:
		it, err = p.S.Search(ctx, q, kind)
	}
	if err != nil {
		return nil, err
	}
	if err := p.S.Play(ctx, it); err != nil {
		return nil, err
	}
	if on, _ := args["shuffle"].(bool); on {
		if err := p.S.SetShuffle(ctx, true); err != nil {
			log.Printf("spotify: shuffle: %v", err)
		}
	}
	res := map[string]any{"playing": it.Name, "kind": it.Kind}
	if it.Artist != "" {
		res["artist"] = it.Artist
	}
	return res, nil
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
