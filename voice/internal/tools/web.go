package tools

import (
	"context"
	"fmt"
	"strings"

	"voice/internal/agent"
)

// Web answers a question from a web search. It is a separate model request,
// since built-in search and function declarations together are not reliable
// across models.
type Web struct {
	LLM interface {
		Stream(ctx context.Context, r agent.Request, onText func(string)) (agent.Content, error)
	}
	Model func() string // nil or "" = the client's
}

const webSystem = "Answer the question using web search, in 2 sentences, for a voice reply: in Russian, plain text, no lists or markdown."

func (Web) Decl() agent.FuncDecl {
	return agent.FuncDecl{
		Name:        "web_answer",
		Description: "Look up current facts on the web (news, scores, prices, anything recent or unknown) and get a short answer.",
		Params: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"question": map[string]any{"type": "string"},
			},
			"required": []string{"question"},
		},
	}
}

func (w Web) Run(ctx context.Context, args map[string]any) (map[string]any, error) {
	q, _ := args["question"].(string)
	if strings.TrimSpace(q) == "" {
		return nil, fmt.Errorf("пустой вопрос")
	}
	req := agent.Request{
		System:       webSystem,
		Contents:     []agent.Content{{Role: "user", Parts: []agent.Part{{Text: q}}}},
		GoogleSearch: true,
	}
	if w.Model != nil {
		req.Model = w.Model()
	}
	c, err := w.LLM.Stream(ctx, req, func(string) {})
	if err != nil {
		return nil, err
	}
	var sb strings.Builder
	for _, p := range c.Parts {
		sb.WriteString(p.Text)
	}
	return map[string]any{"answer": strings.TrimSpace(sb.String())}, nil
}
