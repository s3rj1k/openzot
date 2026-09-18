package provider

import (
	"context"
	"strings"
)

// Complete runs a turn and collects the whole stream, for tests that do not want
// to render it as it arrives. Nothing in a run needs it: the engine consumes the
// stream as it arrives.
func (c *Client) Complete(ctx context.Context, request Request) (ChatMessage, string, *Usage, error) {
	var (
		text      textBuilder
		reasoning textBuilder
		calls     []ToolCall
		finish    string
		usage     *Usage
	)

	for event := range c.Stream(ctx, request) {
		if event.Err != nil {
			return ChatMessage{}, "", nil, event.Err
		}

		text.WriteString(event.Token)
		reasoning.WriteString(event.ReasoningToken)

		if event.FinishReason != "" {
			finish = event.FinishReason
		}

		if len(event.ToolCalls) > 0 {
			calls = event.ToolCalls
		}

		if event.Usage != nil {
			usage = event.Usage
		}
	}

	message := ChatMessage{
		Role:             RoleAssistant,
		Content:          text.String(),
		ReasoningContent: reasoning.String(),
		ToolCalls:        calls,
	}

	return message, finish, usage, nil
}

// textBuilder accumulates streamed fragments.
type textBuilder struct {
	parts []string
}

func (b *textBuilder) WriteString(s string) {
	if s != "" {
		b.parts = append(b.parts, s)
	}
}

func (b *textBuilder) String() string {
	return strings.Join(b.parts, "")
}
