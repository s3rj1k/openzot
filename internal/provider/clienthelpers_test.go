package provider_test

import (
	"context"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/openzot/openzot/internal/provider"
)

// turn is what one model call produced.
type turn struct {
	text      string
	reasoning string
	calls     []fantasy.ToolCallContent
	finish    fantasy.FinishReason
	usage     fantasy.Usage
	err       error
}

// streamOf runs one model call. A failure to start it arrives as an error part,
// the same way a failure mid-stream does, so a caller has one place to look.
func streamOf(ctx context.Context, c *provider.Client, call *fantasy.Call) fantasy.StreamResponse {
	return func(yield func(fantasy.StreamPart) bool) {
		stream, err := c.Model().Stream(ctx, *call)
		if err != nil {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: err})

			return
		}

		for part := range stream {
			if !yield(part) {
				return
			}
		}
	}
}

// collect runs one call to the end.
func collect(client *provider.Client, call *fantasy.Call) turn {
	var result turn

	for part := range streamOf(context.Background(), client, call) {
		switch part.Type {
		case fantasy.StreamPartTypeTextDelta:
			result.text += part.Delta
		case fantasy.StreamPartTypeReasoningDelta:
			result.reasoning += part.Delta
		case fantasy.StreamPartTypeToolCall:
			result.calls = append(result.calls, fantasy.ToolCallContent{
				ToolCallID: part.ID, ToolName: part.ToolCallName, Input: part.ToolCallInput,
			})
		case fantasy.StreamPartTypeFinish:
			result.finish = part.FinishReason
			result.usage = part.Usage
		case fantasy.StreamPartTypeError:
			result.err = part.Error
		default:
			// the other parts carry nothing these tests read
		}
	}

	return result
}

// hello is the smallest valid call.
func hello() *fantasy.Call {
	return &fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hi")}}
}

// withStallTimeout shortens the silence bound for a test.
func withStallTimeout(t *testing.T, timeout time.Duration) {
	t.Helper()

	previous := provider.StreamStallTimeout
	provider.StreamStallTimeout = timeout

	t.Cleanup(func() { provider.StreamStallTimeout = previous })
}
