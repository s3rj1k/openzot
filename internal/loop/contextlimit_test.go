package loop

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/conversation"
	"github.com/openzot/openzot/internal/provider"
)

// The context-limit recovery path. A provider rejecting an oversized prompt is
// not a failure, it is a signal to trim harder and try again. The conversation
// itself is never rewritten - only the window the oldest messages are forgotten to fit.

// contextLimitOnce rejects the first request with a context-length error and
// serves a normal turn afterwards.
func contextLimitOnce(t *testing.T) (*provider.Client, *int) {
	t.Helper()

	requests := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++

		if requests == 1 {
			w.WriteHeader(http.StatusBadRequest)

			_ = json.NewEncoder(w).Encode(map[string]any{
				litError: map[string]any{
					litMessage: "This model's maximum context length is 8192 tokens, however you requested 9000",
				},
			})

			return
		}

		w.Header().Set("Content-Type", "text/event-stream")

		fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"d","type":"function","function":{"name":"success","arguments":"{\"summary\":\"recovered\"}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))

	t.Cleanup(server.Close)

	client, err := provider.NewClient(t.Context(), provider.ClientConfig{
		Provider: litCustom,
		Model:    litTestModel,
		APIKey:   "k",
		BaseURL:  server.URL,
	})
	require.NoError(t, err)

	return client, &requests
}

// longConversation builds enough history to be worth trimming.
func longConversation(turns int) []conversation.Message {
	messages := make([]conversation.Message, 0, turns)

	for index := range turns {
		kind := conversation.TypeUser

		if index%2 == 1 {
			kind = conversation.TypeBot
		}

		messages = append(messages, conversation.Message{
			Type: kind,
			Text: fmt.Sprintf("turn %d: %s", index, strings.Repeat("padding ", 200)),
		})
	}

	return messages
}

func TestContextLimitNarrowsTheBudgetAndRetries(t *testing.T) {
	client, requests := contextLimitOnce(t)

	engine, err := New(&Options{
		ContextWindow: testWindow,
		Client:        client,
		Messages:      longConversation(40),
	})
	require.NoError(t, err)

	// the configured window, far above the 8192 the provider states
	engine.window = 40_000

	result := engine.Run(t.Context(), nil)

	require.Equal(t, StopSettled, result.Reason, "want the run to recover and stop normally")

	assert.GreaterOrEqual(t, *requests, 2, "the request was not retried after the rejection (%d requests)", *requests)

	assert.Equal(t, 1, result.Budget.Recoveries, "want the rejection to count as one")

	// 85% of the stated 8192
	assert.Equal(t, 6963, engine.window)
}

// Trimming happens on the wire, not in the history. Every message the run
// started with is still in the conversation afterwards, and nothing has been
// summarized into its place.
func TestContextLimitNeverRewritesTheConversation(t *testing.T) {
	client, _ := contextLimitOnce(t)

	original := longConversation(40)

	engine, err := New(&Options{ContextWindow: testWindow, Client: client, Messages: original})
	require.NoError(t, err)

	engine.window = 40_000

	result := engine.Run(t.Context(), nil)

	require.GreaterOrEqual(t, len(result.Messages), len(original), "the conversation shrank from %d to %d messages", len(original), len(result.Messages))

	for index, message := range original {
		got := result.Messages[index]

		require.Equal(t, message.Type, got.Type, "message %d was rewritten", index)
		require.Equal(t, message.Text, got.Text, "message %d was rewritten", index)
	}
}

// A provider that keeps saying "too long" is wrong about its own ceiling only so
// far. Once the window is down to a fraction of the configured one there is
// nothing left to try, and retrying would send the same request again.
func TestNarrowingStopsAtTheFloor(t *testing.T) {
	client, _ := contextLimitOnce(t)

	engine, err := New(&Options{ContextWindow: testWindow, Client: client, Messages: longConversation(4)})
	require.NoError(t, err)

	floor := testWindow / narrowFloor

	engine.window = floor

	// no stated window, so the only move is stepping the window down
	assert.False(t, engine.narrowWindow(provider.ContextLimit{}, func(Event) {}), "the window narrowed to %d, below the %d floor", engine.window, floor)

	assert.Equal(t, floor, engine.window)
}

// A rejection without a stated window steps the budget down by a quarter.
func TestNarrowingWithoutAStatedWindowStepsDown(t *testing.T) {
	client, _ := contextLimitOnce(t)

	engine, err := New(&Options{ContextWindow: 40_000, Client: client})
	require.NoError(t, err)

	require.True(t, engine.narrowWindow(provider.ContextLimit{}, func(Event) {}))

	assert.Equal(t, 30_000, engine.window)
}

// A context limit that persists is eventually a real failure rather than an
// infinite retry loop.
func TestPersistentContextLimitGivesUp(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)

		_ = json.NewEncoder(w).Encode(map[string]any{
			litError: map[string]any{litMessage: "maximum context length exceeded"},
		})
	}))

	defer server.Close()

	client, err := provider.NewClient(t.Context(), provider.ClientConfig{
		Provider: litCustom,
		Model:    litTestModel,
		APIKey:   "k",
		BaseURL:  server.URL,
	})
	require.NoError(t, err)

	engine, err := New(&Options{
		ContextWindow:    testWindow,
		Client:           client,
		Messages:         longConversation(40),
		MaxContinuations: 3,
	})
	require.NoError(t, err)

	engine.window = 40_000

	result := engine.Run(t.Context(), nil)

	assert.Equal(t, StopError, result.Reason, "want error once narrowing stops helping")

	require.Error(t, result.Err, "the underlying provider error must be reported")
}

// A transient provider failure is retried rather than ending the run.
func TestRetriableProviderErrorIsRetried(t *testing.T) {
	requests := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++

		if requests == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)

			fmt.Fprint(w, `{"error":{"message":"Service temporarily unavailable"}}`)

			return
		}

		w.Header().Set("Content-Type", "text/event-stream")

		fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"d","type":"function","function":{"name":"success","arguments":"{\"summary\":\"second time lucky\"}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))

	defer server.Close()

	client, err := provider.NewClient(t.Context(), provider.ClientConfig{
		Provider: litCustom,
		Model:    litTestModel,
		APIKey:   "k",
		BaseURL:  server.URL,
	})
	require.NoError(t, err)

	engine, err := New(&Options{
		ContextWindow: testWindow,
		Client:        client,
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		RetryBackoff:  -1, // the retry itself is under test, not its pacing
	})
	require.NoError(t, err)

	var retried bool

	result := engine.Run(t.Context(), func(event Event) {
		if event.Kind == EventRetry {
			retried = true
		}
	})

	assert.Equal(t, StopSettled, result.Reason)

	assert.True(t, retried, "a retry must be visible to the caller")
}

// A 4xx that is not a context limit is terminal. Retrying a bad key or a missing
// model only burns the budget.
func TestNonRetriableErrorEndsTheRun(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)

		fmt.Fprint(w, `{"error":{"message":"invalid api key"}}`)
	}))

	defer server.Close()

	client, err := provider.NewClient(t.Context(), provider.ClientConfig{
		Provider: litCustom,
		Model:    litTestModel,
		APIKey:   "k",
		BaseURL:  server.URL,
	})
	require.NoError(t, err)

	engine, err := New(&Options{
		ContextWindow: testWindow,
		Client:        client,
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
	})
	require.NoError(t, err)

	result := engine.Run(t.Context(), nil)

	assert.Equal(t, StopError, result.Reason)

	assert.Equal(t, 0, result.Budget.Recoveries, "want no retries for a credential problem")
}

// When a provider rejects for length it states the real window. That is ground
// truth in a way the configured window may not be, so the retry budgets against it
// rather than guessing again.
func TestContextLimitAdoptsTheProviderStatedWindow(t *testing.T) {
	requests := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++

		if requests == 1 {
			w.WriteHeader(http.StatusBadRequest)

			_ = json.NewEncoder(w).Encode(map[string]any{
				litError: map[string]any{
					litMessage: "This model's maximum context length is 8192 tokens. However, your messages resulted in 40000 tokens.",
				},
			})

			return
		}

		w.Header().Set("Content-Type", "text/event-stream")

		fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"d","type":"function","function":{"name":"success","arguments":"{\"summary\":\"fits now\"}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))

	defer server.Close()

	client, err := provider.NewClient(t.Context(), provider.ClientConfig{
		Provider: litCustom,
		Model:    litTestModel,
		APIKey:   "k",
		BaseURL:  server.URL,
	})
	require.NoError(t, err)

	engine, err := New(&Options{
		ContextWindow: testWindow,
		Client:        client,
		Messages:      longConversation(40),
	})
	require.NoError(t, err)

	result := engine.Run(t.Context(), nil)

	require.Equal(t, StopSettled, result.Reason)

	// 85% of the stated 8192
	assert.Equal(t, 6963, engine.window, "window")
}

// A rejection with no number still recovers, using the engine's own estimate.
func TestContextLimitWithoutANumberStillRecovers(t *testing.T) {
	requests := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++

		if requests == 1 {
			w.WriteHeader(http.StatusBadRequest)

			_ = json.NewEncoder(w).Encode(map[string]any{
				litError: map[string]any{litMessage: "prompt is too long"},
			})

			return
		}

		w.Header().Set("Content-Type", "text/event-stream")

		fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"d","type":"function","function":{"name":"success","arguments":"{\"summary\":\"ok\"}"}}]},"finish_reason":"tool_calls"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))

	defer server.Close()

	client, _ := provider.NewClient(t.Context(), provider.ClientConfig{
		Provider: litCustom,
		Model:    litTestModel,
		APIKey:   "k",
		BaseURL:  server.URL,
	})

	engine, err := New(&Options{ContextWindow: 40_000, Client: client, Messages: longConversation(40)})
	require.NoError(t, err)

	assert.Equal(t, StopSettled, engine.Run(t.Context(), nil).Reason)
}
