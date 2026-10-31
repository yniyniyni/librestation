// Package judge asks an audio model (Gemini) whether a keyword hit was a
// person giving the command or a false alarm: song lyrics, TV, other speech,
// noise. It only labels hits; the action has long run by then.
//
// Only the hit's clip (a few seconds of the mic after echo cancellation)
// leaves the speaker, and only when a key is configured.
package judge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"voice/internal/wav"
)

const (
	// DefaultModel is the cheapest current Flash-Lite model that takes audio.
	DefaultModel = "gemini-3.5-flash-lite"
	// DefaultURL is the Gemini API base.
	DefaultURL = "https://generativelanguage.googleapis.com/v1beta"
	timeout    = 30 * time.Second
)

// Verdict is the model's call on one hit.
type Verdict struct {
	Command    bool    `json:"command"`    // a person said the word to the speaker
	Confidence float64 `json:"confidence"` // 0..1
	Heard      string  `json:"heard"`      // what speech the model heard, if any
	Reason     string  `json:"reason"`     // one short sentence
}

// Hit is what the judge is told about a detection.
type Hit struct {
	Word     string  // the command word as the user spelled it, e.g. "стоп"
	Action   string  // pause, play, next, prev, louder, quieter or ""
	Audio    []int16 // the clip, ending at the detection
	Rate     int     // sample rate of Audio
	Undone   float64 // seconds until the user took the action back; 0 = not
	Playback bool    // music was playing when the word was heard
}

// Client calls the Gemini API. The zero value is not usable; see New.
type Client struct {
	Key   string
	Model string
	URL   string
	HTTP  *http.Client
}

// New returns a client for key with the default model and endpoint.
func New(key string) *Client {
	return &Client{Key: key, Model: DefaultModel, URL: DefaultURL, HTTP: &http.Client{Timeout: timeout}}
}

// ReadKey reads an API key from path; a missing file is no key, not an error.
func ReadKey(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return strings.TrimSpace(string(b)), err
}

const prompt = `This audio clip was recorded by the microphone of a smart speaker, after echo cancellation, and ends at the moment its on-device keyword spotter detected the Russian command word «%s» (action: %s).
The speaker has no wake word: the word alone triggers the action, so false alarms come from song lyrics or speech played by the speaker itself and only partly removed by echo cancellation, from TV or video, from people talking that happen to say something similar, and from noise.
%s
Decide whether a person in the room said «%s» (or a clear form of it) as a command to the speaker. A person saying it inside a longer sentence to someone else is not a command. Answer in JSON: command (true only for a real command), confidence (0 to 1), heard (the words you hear near the end of the clip, empty if none), reason (one short sentence in Russian).`

// Judge asks the model about one hit.
func (c *Client) Judge(ctx context.Context, h Hit) (Verdict, error) {
	var hints []string
	if h.Playback {
		hints = append(hints, "Music was playing from the speaker at the time.")
	} else {
		hints = append(hints, "The speaker was not playing music at the time.")
	}
	if h.Undone > 0 {
		hints = append(hints, fmt.Sprintf("The user took the action back %.0f s later, which often means it was unwanted; weigh it, but trust the audio.", h.Undone))
	}
	action := h.Action
	if action == "" {
		action = "none"
	}
	text := fmt.Sprintf(prompt, h.Word, action, strings.Join(hints, " "), h.Word)

	body := map[string]any{
		"contents": []any{map[string]any{
			"role": "user",
			"parts": []any{
				map[string]any{"text": text},
				map[string]any{"inline_data": map[string]any{
					"mime_type": "audio/wav",
					"data":      base64.StdEncoding.EncodeToString(wav.Encode(h.Audio, h.Rate)),
				}},
			},
		}},
		"generationConfig": map[string]any{
			"temperature":      0,
			"responseMimeType": "application/json",
			"responseSchema": map[string]any{
				"type": "OBJECT",
				"properties": map[string]any{
					"command":    map[string]any{"type": "BOOLEAN"},
					"confidence": map[string]any{"type": "NUMBER"},
					"heard":      map[string]any{"type": "STRING"},
					"reason":     map[string]any{"type": "STRING"},
				},
				"required": []string{"command", "confidence", "heard", "reason"},
			},
		},
	}
	b, err := json.Marshal(body)
	if err != nil {
		return Verdict{}, err
	}
	url := fmt.Sprintf("%s/models/%s:generateContent", strings.TrimRight(c.URL, "/"), c.Model)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return Verdict{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", c.Key)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Verdict{}, err
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Verdict{}, err
	}
	var r struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rb, &r); err != nil {
		return Verdict{}, fmt.Errorf("gemini: %s: bad response: %v", resp.Status, err)
	}
	if r.Error != nil {
		return Verdict{}, fmt.Errorf("gemini: %s: %s", r.Error.Status, r.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return Verdict{}, fmt.Errorf("gemini: %s", resp.Status)
	}
	var out strings.Builder
	for _, cand := range r.Candidates {
		for _, p := range cand.Content.Parts {
			out.WriteString(p.Text)
		}
		break
	}
	if out.Len() == 0 {
		reason := "no candidates"
		if len(r.Candidates) > 0 {
			reason = "finish reason " + r.Candidates[0].FinishReason
		}
		return Verdict{}, fmt.Errorf("gemini: empty answer (%s)", reason)
	}
	var v Verdict
	if err := json.Unmarshal([]byte(out.String()), &v); err != nil {
		return Verdict{}, fmt.Errorf("gemini: answer is not the JSON asked for: %v", err)
	}
	v.Confidence = min(max(v.Confidence, 0), 1)
	return v, nil
}
