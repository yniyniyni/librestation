package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// maxRounds bounds the model calls in one turn; each round that asks for
	// tools is followed by another call with their results.
	maxRounds = 4
	// maxSpoken bounds the characters of a spoken reply.
	maxSpoken = 400
	// audioRate is the rate of the captured request audio.
	audioRate = 16000
	// doneReply and failedReply are said when the model ends a tool round
	// with nothing to say: the tools worked, or one of them failed.
	doneReply   = "Готово."
	failedReply = "Не получилось."
)

var errTooManyRounds = errors.New("agent: too many tool rounds")

// Tool is something the model can ask for.
type Tool interface {
	Decl() FuncDecl
	// Run executes the call. An error is handed to the model as
	// {"error": ...} so it can tell the user.
	Run(ctx context.Context, args map[string]any) (map[string]any, error)
}

// Streamer is the model; *Client implements it.
type Streamer interface {
	Stream(ctx context.Context, r Request, onText func(string)) (Content, error)
}

// Agent answers one request at a time.
type Agent struct {
	Model func() string // model name; nil or "" = the client's
	LLM   Streamer
	Tools []Tool
	Now   func() time.Time // nil = time.Now
}

// Outcome is what a turn came to.
type Outcome struct {
	NoRequest         bool     // the audio held no request for the speaker
	StartConversation bool     // the user wants a dialogue
	Spoken            string   // what went to say
	Calls             []string // tools run, in order
}

// The agent's own pseudo-tools.
var pseudo = []FuncDecl{
	{Name: "no_request", Description: "Call when the audio contains no request addressed to the speaker (silence, background music, talk to other people). Say nothing."},
	{Name: "start_conversation", Description: "Call when the user wants a free-form conversation rather than a single command."},
}

func isPseudo(name string) bool {
	for _, d := range pseudo {
		if d.Name == name {
			return true
		}
	}
	return false
}

// Turn feeds the user's audio to the model, runs the tools it asks for and
// passes the final reply to say sentence by sentence. It returns hist
// extended with this turn, for the follow-up.
func (a *Agent) Turn(ctx context.Context, hist []Content, audio []int16, say func(sentence string)) ([]Content, Outcome, error) {
	var out Outcome
	now := time.Now
	if a.Now != nil {
		now = a.Now
	}
	model := ""
	if a.Model != nil {
		model = a.Model()
	}
	funcs := append([]FuncDecl(nil), pseudo...)
	byName := map[string]Tool{}
	for _, t := range a.Tools {
		d := t.Decl()
		funcs = append(funcs, d)
		byName[d.Name] = t
	}
	system := prompt(now())

	contents := append(append([]Content(nil), hist...),
		Content{Role: "user", Parts: []Part{{Audio: audio, AudioRate: audioRate}}})
	var spoken []string
	total, capped := 0, false
	toolOK, toolFailed := false, false // a real tool ran with or without error
	for round := 0; round < maxRounds; round++ {
		// Sentences are spoken as they complete; a round that turns out to
		// call tools drops its unfinished tail.
		sp := &speaker{say: func(s string) {
			n := utf8.RuneCountInString(s)
			if capped || total+n > maxSpoken {
				capped = true // later, shorter sentences must not slip past the cut
				return
			}
			total += n
			spoken = append(spoken, s)
			say(s)
		}}
		reply, err := a.LLM.Stream(ctx, Request{System: system, Contents: contents, Funcs: funcs, Model: model}, sp.write)
		var empty *EmptyAnswerError
		if round > 0 && errors.As(err, &empty) && empty.Reason == "STOP" {
			// Gemini sometimes answers a tool result with nothing: the
			// tools have done the job.
			if (toolOK || toolFailed) && len(spoken) == 0 {
				line := doneReply
				if toolFailed {
					line = failedReply
				}
				sp.say(line)
				contents = append(contents, Content{Role: "model", Parts: []Part{{Text: line}}})
			}
			out.Spoken = strings.Join(spoken, " ")
			return contents, out, nil
		}
		if err != nil {
			return contents, out, err
		}
		contents = append(contents, reply)
		var calls []Part
		for _, p := range reply.Parts {
			if p.Call != nil {
				calls = append(calls, p)
			}
		}
		if len(calls) == 0 {
			sp.flush()
			out.Spoken = strings.Join(spoken, " ")
			return contents, out, nil
		}
		if round == maxRounds-1 {
			return contents, out, errTooManyRounds
		}
		results := Content{Role: "user"}
		for _, p := range calls {
			c := p.Call
			var resp map[string]any
			switch {
			case isPseudo(c.Name):
				resp = map[string]any{"ok": true}
				if c.Name == "no_request" {
					out.NoRequest = true
				} else {
					out.StartConversation = true
				}
			case byName[c.Name] == nil:
				resp = map[string]any{"error": "unknown tool " + c.Name}
			default:
				out.Calls = append(out.Calls, c.Name)
				r, err := byName[c.Name].Run(ctx, c.Args)
				logCall(c.Name, c.Args, err)
				if err != nil {
					r = map[string]any{"error": err.Error()}
					toolFailed = true
				} else {
					toolOK = true
				}
				resp = r
			}
			results.Parts = append(results.Parts, Part{Result: &Result{Name: c.Name, Response: resp}})
		}
		contents = append(contents, results)
		if out.NoRequest || out.StartConversation {
			return contents, out, nil
		}
	}
	return contents, out, errTooManyRounds
}

// logCall logs one tool call: its name, its args as at most 80 characters of
// JSON, and ok or the error.
func logCall(name string, args map[string]any, err error) {
	b, _ := json.Marshal(args)
	a := []rune(string(b))
	if len(a) > 80 {
		a = append(a[:80], '…')
	}
	res := "ok"
	if err != nil {
		res = err.Error()
	}
	log.Printf("assistant: tool %s %s: %s", name, string(a), res)
}

// speaker cuts streamed text into sentences.
type speaker struct {
	buf string
	say func(string)
}

func (s *speaker) write(delta string) {
	s.buf += delta
	runes := []rune(s.buf)
	start := 0
	for i := 0; i+1 < len(runes); i++ {
		if isStop(runes[i]) && runes[i+1] == ' ' {
			s.emit(string(runes[start : i+1]))
			start = i + 2
		}
	}
	s.buf = string(runes[start:])
}

// flush speaks what is left once the stream has ended.
func (s *speaker) flush() {
	s.emit(s.buf)
	s.buf = ""
}

func (s *speaker) emit(sentence string) {
	if sentence = strings.TrimSpace(sentence); sentence != "" {
		s.say(sentence)
	}
}

func isStop(r rune) bool {
	return r == '.' || r == '!' || r == '?' || r == '…'
}
