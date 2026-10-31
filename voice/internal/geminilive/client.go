// Package geminilive implements the small subset of Google's Live wire protocol
// used by the station. It does not execute tools or retry side effects.
package geminilive

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const DefaultModel = "gemini-3.8-live"
const Endpoint = "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"

type Declaration struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
	Behavior    string         `json:"behavior,omitempty"`
}
type Config struct {
	Key, Model, Voice, Prompt, URL string
	Tools                          []Declaration
	HTTP                           *http.Client // nil uses DefaultClient, including environment proxy settings
}
type FunctionCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}
type FunctionResponse struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}
type Blob struct {
	MimeType string `json:"mimeType"`
	Data     []byte `json:"data"`
}
type Part struct {
	InlineData *Blob  `json:"inlineData,omitempty"`
	Text       string `json:"text,omitempty"`
}
type Content struct {
	Parts []Part `json:"parts"`
}
type Transcription struct {
	Text string `json:"text"`
}
type ServerContent struct {
	ModelTurn           *Content       `json:"modelTurn,omitempty"`
	Interrupted         bool           `json:"interrupted"`
	TurnComplete        bool           `json:"turnComplete"`
	InputTranscription  *Transcription `json:"inputTranscription,omitempty"`
	OutputTranscription *Transcription `json:"outputTranscription,omitempty"`
}
type Message struct {
	SetupComplete *struct{}      `json:"setupComplete,omitempty"`
	ServerContent *ServerContent `json:"serverContent,omitempty"`
	ToolCall      *struct {
		FunctionCalls []FunctionCall `json:"functionCalls"`
	} `json:"toolCall,omitempty"`
	ToolCallCancellation *struct {
		IDs []string `json:"ids"`
	} `json:"toolCallCancellation,omitempty"`
	VoiceActivity *struct {
		Type string `json:"type"`
	} `json:"voiceActivity,omitempty"`
	GoAway *struct {
		TimeLeft string `json:"timeLeft"`
	} `json:"goAway,omitempty"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type Client struct {
	conn *websocket.Conn
	key  string
}

func Dial(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Key == "" {
		return nil, fmt.Errorf("live: missing API key")
	}
	if cfg.URL == "" {
		cfg.URL = Endpoint
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.Voice == "" {
		cfg.Voice = "Kore"
	}
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("live: invalid endpoint")
	}
	q := u.Query()
	q.Set("key", cfg.Key)
	u.RawQuery = q.Encode()
	setupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, res, err := websocket.Dial(setupCtx, u.String(), &websocket.DialOptions{HTTPClient: cfg.HTTP})
	if err != nil {
		// Dial errors include the authenticated URL. Never propagate it into logs/UI.
		if res != nil {
			return nil, fmt.Errorf("live: connection refused (HTTP %d)", res.StatusCode)
		}
		return nil, fmt.Errorf("live: connection failed")
	}
	conn.SetReadLimit(4 << 20)
	c := &Client{conn: conn, key: cfg.Key}
	setup := map[string]any{
		"model":                   "models/" + strings.TrimPrefix(cfg.Model, "models/"),
		"generationConfig":        map[string]any{"responseModalities": []string{"AUDIO"}, "speechConfig": map[string]any{"voiceConfig": map[string]any{"prebuiltVoiceConfig": map[string]any{"voiceName": cfg.Voice}}}},
		"systemInstruction":       map[string]any{"parts": []any{map[string]any{"text": cfg.Prompt}}},
		"inputAudioTranscription": map[string]any{}, "outputAudioTranscription": map[string]any{},
		// The station already segments each utterance with its local VAD.
		// Disable server VAD and use explicit activity boundaries below.
		"realtimeInputConfig": map[string]any{"automaticActivityDetection": map[string]any{"disabled": true}},
	}
	if len(cfg.Tools) > 0 {
		setup["tools"] = []any{map[string]any{"functionDeclarations": cfg.Tools}}
	}
	if err = c.write(setupCtx, map[string]any{"setup": setup}); err != nil {
		c.Close()
		return nil, err
	}
	msg, err := c.Receive(setupCtx)
	if err == nil && msg.SetupComplete == nil {
		err = fmt.Errorf("live: missing setupComplete")
	}
	if err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// A single writer timeout covers both a stalled proxy and a stalled service.
func (c *Client) write(ctx context.Context, v any) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, c.conn, v); err != nil {
		return c.safe(err)
	}
	return nil
}
func (c *Client) safe(err error) error {
	s := strings.ReplaceAll(err.Error(), c.key, "[redacted]")
	s = strings.ReplaceAll(s, url.QueryEscape(c.key), "[redacted]")
	return fmt.Errorf("live: %s", s)
}
func (c *Client) SendAudio(ctx context.Context, pcm []int16) error {
	b := make([]byte, len(pcm)*2)
	for i, s := range pcm {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(s))
	}
	if err := c.write(ctx, map[string]any{"realtimeInput": map[string]any{"activityStart": map[string]any{}}}); err != nil {
		return err
	}
	if err := c.write(ctx, map[string]any{"realtimeInput": map[string]any{"audio": Blob{MimeType: "audio/pcm;rate=16000", Data: b}}}); err != nil {
		return err
	}
	return c.write(ctx, map[string]any{"realtimeInput": map[string]any{"activityEnd": map[string]any{}}})
}
func (c *Client) Respond(ctx context.Context, r []FunctionResponse) error {
	return c.write(ctx, map[string]any{"toolResponse": map[string]any{"functionResponses": r}})
}
func (c *Client) Receive(ctx context.Context) (Message, error) {
	var m Message
	if err := wsjson.Read(ctx, c.conn, &m); err != nil {
		return m, c.safe(err)
	}
	if m.Error != nil {
		return m, c.safe(fmt.Errorf("API %d: %s", m.Error.Code, m.Error.Message))
	}
	return m, nil
}
func (c *Client) Close() { c.conn.CloseNow() }
