package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// serve stands up a fake endpoint and a client pointed at it.
func serve(t *testing.T, handler http.HandlerFunc, tweak ...func(*ClientConfig)) *Client {
	t.Helper()

	server := httptest.NewServer(handler)

	t.Cleanup(server.Close)

	config := ClientConfig{Provider: "test", Model: litTestModel, APIKey: "test-key", BaseURL: server.URL}

	for _, change := range tweak {
		change(&config)
	}

	client, err := NewClient(t.Context(), config)
	require.NoError(t, err)

	return client
}

// sse renders frames as an event stream, one blank line between events.
func sse(lines ...string) string {
	var body strings.Builder

	for _, line := range lines {
		body.WriteString("data: " + line + "\n\n")
	}

	body.WriteString("data: [DONE]\n\n")

	return body.String()
}

// frames serves a fixed SSE body, closed the way a real server closes it.
func frames(t *testing.T, lines ...string) *Client {
	t.Helper()

	return serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		_, _ = w.Write([]byte(sse(lines...)))
	})
}

// turn is what one model call produced.
type turn struct {
	text      string
	reasoning string
	calls     []fantasy.ToolCallContent
	finish    fantasy.FinishReason
	usage     fantasy.Usage
	err       error
}

// Stream runs one model call. A failure to start it arrives as an error part,
// the same way a failure mid-stream does, so a caller has one place to look.
func (c *Client) Stream(ctx context.Context, call *fantasy.Call) fantasy.StreamResponse {
	return func(yield func(fantasy.StreamPart) bool) {
		stream, err := c.model.Stream(ctx, *call)
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
func collect(client *Client, call *fantasy.Call) turn {
	var result turn

	for part := range client.Stream(context.Background(), call) {
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

	previous := streamStallTimeout
	streamStallTimeout = timeout

	t.Cleanup(func() { streamStallTimeout = previous })
}
