package tts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"voice/internal/judge"
)

const (
	// DefaultTTSModel is the Flash model that returns speech.
	// TODO(verify): current Flash TTS model id
	DefaultTTSModel = "gemini-2.5-flash-preview-tts"
	DefaultVoice    = "Kore"
	defaultRate     = 24000
	geminiTimeout   = 20 * time.Second
)

// GeminiTTS speaks through the Gemini generateContent API.
type GeminiTTS struct {
	Key, Model, Voice, URL string
	HTTP                   *http.Client
}

// NewGeminiTTS returns a client with the default model, voice and URL.
func NewGeminiTTS(key string) *GeminiTTS {
	return &GeminiTTS{Key: key, Model: DefaultTTSModel, Voice: DefaultVoice, URL: judge.DefaultURL,
		HTTP: &http.Client{Timeout: geminiTimeout}}
}

type ttsPart struct {
	InlineData *struct {
		MimeType string `json:"mimeType"`
		Data     string `json:"data"`
	} `json:"inlineData"`
}

func (g *GeminiTTS) Synth(ctx context.Context, text string) ([]int16, int, error) {
	model, voice, base, hc := g.Model, g.Voice, g.URL, g.HTTP
	if model == "" {
		model = DefaultTTSModel
	}
	if voice == "" {
		voice = DefaultVoice
	}
	if base == "" {
		base = judge.DefaultURL
	}
	if hc == nil {
		hc = &http.Client{Timeout: geminiTimeout}
	}
	if !judge.ValidModel(model) {
		return nil, 0, fmt.Errorf("gemini: bad model name %q", model)
	}
	body, _ := json.Marshal(map[string]any{
		"contents": []any{map[string]any{"parts": []any{map[string]any{"text": text}}}},
		"generationConfig": map[string]any{
			"responseModalities": []string{"AUDIO"},
			"speechConfig": map[string]any{"voiceConfig": map[string]any{
				"prebuiltVoiceConfig": map[string]any{"voiceName": voice}}},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(base, "/")+"/models/"+model+":generateContent", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", g.Key)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("gemini: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, 0, fmt.Errorf("gemini: %v", err)
	}
	var r struct {
		Candidates []struct {
			Content struct {
				Parts []ttsPart `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		Error *struct{ Status, Message string } `json:"error"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, 0, fmt.Errorf("gemini: %s: bad response: %v", resp.Status, err)
	}
	if r.Error != nil {
		return nil, 0, fmt.Errorf("gemini: %s: %s", r.Error.Status, r.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("gemini: %s", resp.Status)
	}
	if len(r.Candidates) == 0 {
		return nil, 0, errors.New("gemini: no audio in answer")
	}
	for _, part := range r.Candidates[0].Content.Parts {
		if part.InlineData == nil {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
		if err != nil {
			return nil, 0, fmt.Errorf("gemini: bad audio data: %v", err)
		}
		pcm := make([]int16, len(b)/2)
		for i := range pcm {
			pcm[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
		}
		if len(pcm) == 0 {
			break
		}
		return pcm, mimeRate(part.InlineData.MimeType), nil
	}
	return nil, 0, fmt.Errorf("gemini: no audio in answer (%s)", r.Candidates[0].FinishReason)
}

// mimeRate reads rate=N from e.g. "audio/L16;codec=pcm;rate=24000".
func mimeRate(mime string) int {
	for _, f := range strings.Split(mime, ";") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(f), "rate="); ok {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return n
			}
		}
	}
	return defaultRate
}
