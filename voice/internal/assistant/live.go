package assistant

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"voice/internal/agent"
	"voice/internal/geminilive"
)

// LiveSession is the transport used by the Live-first assistant path.
type LiveSession interface {
	SendAudio(context.Context, []int16) error
	Receive(context.Context) (geminilive.Message, error)
	Respond(context.Context, []geminilive.FunctionResponse) error
	Close()
}

// livePlayer streams Live PCM without waiting after every small audio chunk.
// The existing Player interface remains blocking for regular TTS replies.
type livePlayer interface {
	PlayLive(context.Context, []int16, int) error
	FinishLive(context.Context) error
}

var errLiveDone = errors.New("assistant: Live conversation ended")

const LivePrompt = `Ты голосовой помощник умной колонки. Отвечай по-русски коротко и естественно. Используй объявленные инструменты для музыки, громкости, погоды и времени. Не утверждай, что действие выполнено без успешного результата. Если пользователь просит закончить разговор, вызови end_conversation.`

func LiveDeclarations(tools []agent.Tool) []geminilive.Declaration {
	out := make([]geminilive.Declaration, 0, len(tools)+1)
	for _, t := range tools {
		d := t.Decl()
		out = append(out, geminilive.Declaration{Name: d.Name, Description: d.Description, Parameters: d.Params, Behavior: "BLOCKING"})
	}
	out = append(out, geminilive.Declaration{Name: "end_conversation", Description: "Закончить разговор по просьбе пользователя.", Parameters: map[string]any{"type": "OBJECT", "properties": map[string]any{}}, Behavior: "BLOCKING"})
	return out
}

func pcm24(blob *geminilive.Blob) ([]int16, error) {
	if blob == nil || len(blob.Data)%2 != 0 {
		return nil, errors.New("assistant: invalid Live PCM")
	}
	if blob.MimeType != "audio/pcm" && blob.MimeType != "audio/pcm;rate=24000" {
		return nil, fmt.Errorf("assistant: unsupported Live audio %q", blob.MimeType)
	}
	out := make([]int16, len(blob.Data)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(blob.Data[i*2:]))
	}
	return out, nil
}

func (s *Session) runLive(ctx context.Context, gen int, onNoRequest func()) {
	// Keep the existing local endpointer for activation and follow-up boundaries;
	// Live handles understanding, tool calls, and native voice generation.
	first := true
	for {
		limit, state := startLimit, "listening"
		if !first {
			limit, state = followLimit, "followup"
		}
		audio, res := s.listen(ctx, gen, state, limit, first)
		if ctx.Err() != nil || res == epStalled {
			s.idle(gen)
			return
		}
		if res == epNoSpeech {
			if first && onNoRequest != nil && s.current(gen) {
				onNoRequest()
			}
			s.idle(gen)
			return
		}
		log.Printf("assistant: Live input %d ms", len(audio)*1000/16000)
		tctx, cancel := context.WithTimeout(ctx, turnTimeout)
		err := s.liveTurn(tctx, gen, audio)
		cancel()
		if err != nil {
			if errors.Is(err, errLiveDone) {
				s.idle(gen)
				return
			}
			if ctx.Err() == nil {
				log.Printf("assistant: Live: %v", err)
				s.fail(ctx, gen, err)
			} else {
				s.idle(gen)
			}
			return
		}
		if !first {
			first = false
		}
		select {
		case <-ctx.Done():
			s.idle(gen)
			return
		case <-s.after(followDelay):
		}
		first = false
	}
}

func (s *Session) liveTurn(ctx context.Context, gen int, audio []int16) error {
	s.set(gen, "thinking", "thinking")
	if s.d.Live == nil {
		return errors.New("assistant: Live is not configured")
	}
	started := time.Now()
	log.Printf("assistant: Live dial")
	conn, err := s.d.Live(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	log.Printf("assistant: Live connected in %v", time.Since(started).Round(time.Millisecond))
	if err := conn.SendAudio(ctx, audio); err != nil {
		return err
	}
	log.Printf("assistant: Live audio sent in %v", time.Since(started).Round(time.Millisecond))
	s.set(gen, "listening", "listening")
	byName := map[string]agent.Tool{}
	for _, t := range s.d.Agent.Tools {
		byName[t.Decl().Name] = t
	}
	for {
		m, err := conn.Receive(ctx)
		if err != nil {
			return err
		}
		log.Printf("assistant: Live message after %v", time.Since(started).Round(time.Millisecond))
		if m.GoAway != nil {
			return errors.New("Live connection is ending")
		}
		if m.ServerContent != nil {
			c := m.ServerContent
			if c.InputTranscription != nil {
				s.recordLive(gen, c.InputTranscription.Text, "", nil)
			}
			if c.OutputTranscription != nil {
				s.recordLive(gen, "", c.OutputTranscription.Text, nil)
			}
			if c.Interrupted {
				s.set(gen, "listening", "listening")
			}
			if c.ModelTurn != nil {
				s.set(gen, "speaking", "speaking")
				for _, p := range c.ModelTurn.Parts {
					if p.InlineData != nil {
						pcm, e := pcm24(p.InlineData)
						if e != nil {
							return e
						}
						var playErr error
						if lp, ok := s.d.Player.(livePlayer); ok {
							playErr = lp.PlayLive(ctx, pcm, 24000)
						} else {
							playErr = s.d.Player.Play(ctx, pcm, 24000)
						}
						if playErr != nil && ctx.Err() == nil {
							return playErr
						}
					}
				}
			}
			if c.TurnComplete {
				if lp, ok := s.d.Player.(livePlayer); ok {
					if err := lp.FinishLive(ctx); err != nil && ctx.Err() == nil {
						return err
					}
				}
				s.set(gen, "followup", "followup")
				return nil
			}
		}
		if m.ToolCall != nil {
			responses := make([]geminilive.FunctionResponse, 0, len(m.ToolCall.FunctionCalls))
			for _, call := range m.ToolCall.FunctionCalls {
				resp := map[string]any{}
				if call.Name == "end_conversation" {
					resp["ok"] = true
					responses = append(responses, geminilive.FunctionResponse{ID: call.ID, Name: call.Name, Response: resp})
					if err := conn.Respond(ctx, responses); err != nil {
						return err
					}
					s.idle(gen)
					return errLiveDone
				}
				t := byName[call.Name]
				if t == nil {
					resp["error"] = "unknown tool " + call.Name
				} else {
					var args map[string]any
					if err := json.Unmarshal(call.Args, &args); err != nil {
						resp["error"] = "invalid arguments"
					} else if r, e := t.Run(ctx, args); e != nil {
						resp["error"] = e.Error()
					} else {
						resp = r
					}
				}
				responses = append(responses, geminilive.FunctionResponse{ID: call.ID, Name: call.Name, Response: resp})
			}
			if err := conn.Respond(ctx, responses); err != nil {
				return err
			}
		}
	}
}

func (s *Session) recordLive(gen int, input, output string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.gen {
		return
	}
	if input != "" {
		s.st.LastHeard = strings.TrimSpace(s.st.LastHeard + " " + input)
	}
	if output != "" {
		s.st.LastReply = strings.TrimSpace(s.st.LastReply + " " + output)
	}
	if err != nil {
		s.st.LastError = err.Error()
	}
	s.st.At = time.Now()
}
