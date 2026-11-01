// Package agent runs the voice assistant: it talks to Gemini with function
// calling, runs the tools the model asks for and speaks the answer.
package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"voice/internal/judge"
	"voice/internal/wav"
)

// timeout bounds a whole answer, streaming included.
const timeout = 2 * time.Minute

// Part is one piece of a message. Exactly one of Text, Audio, Call and Result
// is set.
type Part struct {
	Text      string
	Audio     []int16 // PCM; in a user part it is sent as WAV
	AudioRate int
	Call      *Call   // the model asks for a tool
	Result    *Result // the answer to a Call, sent back as the user

	// raw is the model's part verbatim, thoughtSignature included. Gemini 3
	// rejects history that drops the signatures, so it goes back unchanged.
	raw json.RawMessage
}

// Content is one message; Role is "user" or "model".
type Content struct {
	Role  string
	Parts []Part
}

// Call is a tool call the model made.
type Call struct {
	Name string
	Args map[string]any
}

// Result is what a tool returned.
type Result struct {
	Name     string
	Response map[string]any
}

// FuncDecl declares a tool; Params is a JSON schema object.
type FuncDecl struct {
	Name, Description string
	Params            map[string]any
}

// Request is one model call.
type Request struct {
	System       string
	Contents     []Content
	Funcs        []FuncDecl
	GoogleSearch bool   // let the model search the web
	Model        string // "" = the client's
}

// Client calls the Gemini API. The zero value is not usable; see NewClient.
type Client struct {
	Key   string
	Model string
	URL   string
	HTTP  *http.Client
}

// NewClient returns a client for key with the default model and endpoint. It
// uses the default transport, so the proxy environment applies.
func NewClient(key string) *Client {
	return &Client{Key: key, Model: judge.DefaultModel, URL: judge.DefaultURL,
		HTTP: &http.Client{Transport: http.DefaultTransport, Timeout: timeout}}
}

// wire is the JSON form of a part as Gemini wants it.
func (p Part) wire() (any, error) {
	switch {
	case p.raw != nil:
		return p.raw, nil
	case p.Call != nil:
		return map[string]any{"functionCall": map[string]any{"name": p.Call.Name, "args": p.Call.Args}}, nil
	case p.Result != nil:
		return map[string]any{"functionResponse": map[string]any{"name": p.Result.Name, "response": p.Result.Response}}, nil
	case p.Audio != nil:
		return map[string]any{"inline_data": map[string]any{
			"mime_type": "audio/wav",
			"data":      base64.StdEncoding.EncodeToString(wav.Encode(p.Audio, p.AudioRate)),
		}}, nil
	case p.Text != "":
		return map[string]any{"text": p.Text}, nil
	}
	return nil, fmt.Errorf("gemini: empty part")
}

// body builds the request JSON.
func (r Request) body() ([]byte, error) {
	var contents []any
	for _, c := range r.Contents {
		var parts []any
		for _, p := range c.Parts {
			w, err := p.wire()
			if err != nil {
				return nil, err
			}
			parts = append(parts, w)
		}
		contents = append(contents, map[string]any{"role": c.Role, "parts": parts})
	}
	body := map[string]any{"contents": contents}
	if r.System != "" {
		body["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": r.System}}}
	}
	var tools []any
	if len(r.Funcs) > 0 {
		var decls []any
		for _, f := range r.Funcs {
			d := map[string]any{"name": f.Name, "description": f.Description}
			if f.Params != nil {
				d["parameters"] = f.Params
			}
			decls = append(decls, d)
		}
		tools = append(tools, map[string]any{"functionDeclarations": decls})
	}
	if r.GoogleSearch {
		tools = append(tools, map[string]any{"google_search": map[string]any{}})
	}
	if tools != nil {
		body["tools"] = tools
	}
	return json.Marshal(body)
}

// chunk is one SSE event.
type chunk struct {
	Candidates []struct {
		Content struct {
			Role  string `json:"role"`
			Parts []struct {
				Text         string `json:"text"`
				Thought      bool   `json:"thought"`
				Signature    string `json:"thoughtSignature"`
				FunctionCall *struct {
					Name string         `json:"name"`
					Args map[string]any `json:"args"`
				} `json:"functionCall"`
				InlineData *struct {
					MimeType string `json:"mimeType"`
					Data     string `json:"data"`
				} `json:"inlineData"`
			} `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	Error *apiError `json:"error"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

// Stream sends r and merges the streamed answer into one model Content,
// calling onText (if not nil) with each text delta as it arrives.
func (c *Client) Stream(ctx context.Context, r Request, onText func(string)) (Content, error) {
	model := c.Model
	if r.Model != "" {
		model = r.Model
	}
	if !judge.ValidModel(model) {
		return Content{}, fmt.Errorf("gemini: bad model name %q", model)
	}
	b, err := r.body()
	if err != nil {
		return Content{}, err
	}
	url := fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse", strings.TrimRight(c.URL, "/"), model)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return Content{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", c.Key)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Content{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var e chunk
		if json.Unmarshal(rb, &e) == nil && e.Error != nil {
			return Content{}, fmt.Errorf("gemini: %s: %s", e.Error.Status, e.Error.Message)
		}
		return Content{}, fmt.Errorf("gemini: %s", resp.Status)
	}

	out := Content{Role: "model"}
	var sigs []string // signature of each merged text part, by index in out.Parts
	var merged []int  // how many chunks went into each part
	var reason string
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" {
			continue
		}
		var ch chunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			return Content{}, fmt.Errorf("gemini: bad stream chunk: %v", err)
		}
		if ch.Error != nil {
			return Content{}, fmt.Errorf("gemini: %s: %s", ch.Error.Status, ch.Error.Message)
		}
		if len(ch.Candidates) == 0 {
			continue
		}
		cand := ch.Candidates[0]
		if cand.FinishReason != "" {
			reason = cand.FinishReason
		}
		// The raw bytes of each part, to keep them verbatim.
		var rawParts struct {
			Candidates []struct {
				Content struct {
					Parts []json.RawMessage `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if err := json.Unmarshal([]byte(data), &rawParts); err != nil {
			return Content{}, fmt.Errorf("gemini: bad stream chunk: %v", err)
		}
		for i, p := range cand.Content.Parts {
			raw := rawParts.Candidates[0].Content.Parts[i]
			part := Part{raw: append(json.RawMessage(nil), raw...)}
			switch {
			case p.FunctionCall != nil:
				part.Call = &Call{Name: p.FunctionCall.Name, Args: p.FunctionCall.Args}
			case p.InlineData != nil:
				pcm, rate, err := decodePCM(p.InlineData.MimeType, p.InlineData.Data)
				if err != nil {
					return Content{}, err
				}
				part.Audio, part.AudioRate = pcm, rate
			case p.Thought:
				// kept for the history, never spoken
			case p.Text != "" || p.Signature != "":
				if onText != nil && p.Text != "" {
					onText(p.Text)
				}
				n := len(out.Parts)
				if n > 0 && isText(out.Parts[n-1]) && sigs[n-1] == "" {
					out.Parts[n-1].Text += p.Text
					sigs[n-1] = p.Signature
					merged[n-1]++
					continue
				}
				part.Text = p.Text
				out.Parts = append(out.Parts, part)
				sigs = append(sigs, p.Signature)
				merged = append(merged, 1)
				continue
			}
			out.Parts = append(out.Parts, part)
			sigs = append(sigs, "")
			merged = append(merged, 1)
		}
	}
	if err := sc.Err(); err != nil {
		return Content{}, err
	}
	// A text part built from several chunks has no single raw form; rebuild it.
	for i := range out.Parts {
		if merged[i] > 1 {
			m := map[string]any{"text": out.Parts[i].Text}
			if sigs[i] != "" {
				m["thoughtSignature"] = sigs[i]
			}
			out.Parts[i].raw, _ = json.Marshal(m)
		}
	}
	if len(out.Parts) == 0 {
		return Content{}, &EmptyAnswerError{Reason: reason}
	}
	return out, nil
}

// EmptyAnswerError is an answer with no parts. Reason is the finish reason,
// "" when no candidate came.
type EmptyAnswerError struct{ Reason string }

func (e *EmptyAnswerError) Error() string {
	if e.Reason == "" {
		return "gemini: empty answer (no candidates)"
	}
	return "gemini: empty answer (finish reason " + e.Reason + ")"
}

// isText says whether p is a plain text part.
func isText(p Part) bool {
	return p.Text != "" && p.Call == nil && p.Result == nil && p.Audio == nil
}

// decodePCM reads a base64 audio/L16 inline part, e.g. "audio/L16;rate=24000".
func decodePCM(mime, data string) ([]int16, int, error) {
	rate := 24000
	if _, after, ok := strings.Cut(mime, "rate="); ok {
		if n, err := fmt.Sscanf(after, "%d", &rate); n != 1 || err != nil {
			rate = 24000
		}
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, 0, fmt.Errorf("gemini: bad audio: %v", err)
	}
	pcm := make([]int16, len(raw)/2)
	for i := range pcm {
		pcm[i] = int16(uint16(raw[2*i]) | uint16(raw[2*i+1])<<8)
	}
	return pcm, rate, nil
}
