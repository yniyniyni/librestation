// Package tools holds the tools the assistant agent can call.
package tools

import (
	"context"
	"time"

	"voice/internal/agent"
)

// Clock tells the current time and date.
type Clock struct {
	Now func() time.Time // nil = time.Now
}

func (Clock) Decl() agent.FuncDecl {
	return agent.FuncDecl{
		Name:        "get_time",
		Description: "Get the current local time and date. Use when the user asks what time or day it is.",
	}
}

func (c Clock) Run(_ context.Context, _ map[string]any) (map[string]any, error) {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	date, clock := agent.RussianDateTime(now())
	return map[string]any{"time": clock, "date": date}, nil
}
