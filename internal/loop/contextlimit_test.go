package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openzot/openzot/internal/llm"
)

// The context-limit recovery path: a provider rejecting an oversized prompt is
// not a failure, it is a signal to trim harder and try again. The conversation
// itself is never rewritten - only the budget the thread builder trims it to.

// contextLimitOnce rejects the first request with a context-length error and
// serves a normal turn afterwards.
func contextLimitOnce(t *testing.T) (*llm.Client, *int) {
	t.Helper()

	requests := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++

		if requests == 1 {
			w.WriteHeader(http.StatusBadRequest)

			json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"message": "This model's maximum context length is 8192 tokens, however you requested 9000",
				},
			})

			return
		}

		w.Header().Set("Content-Type", "text/event-stream")

		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"recovered"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))

	t.Cleanup(server.Close)

	client, err := llm.New(llm.Config{
		Provider: "custom",
		Model:    "test-model",
		APIKey:   "k",
		BaseURL:  server.URL,
	})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}

	return client, &requests
}

// longConversation builds enough history to be worth trimming.
func longConversation(turns int) []Message {
	messages := make([]Message, 0, turns)

	for index := 0; index < turns; index++ {
		kind := TypeUser

		if index%2 == 1 {
			kind = TypeBot
		}

		messages = append(messages, Message{
			Type: kind,
			Text: fmt.Sprintf("turn %d: %s", index, strings.Repeat("padding ", 200)),
		})
	}

	return messages
}

func TestContextLimitNarrowsTheBudgetAndRetries(t *testing.T) {
	client, requests := contextLimitOnce(t)

	engine, err := New(Options{
		Client:   client,
		Messages: longConversation(40),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// the catalogue's guess, far above the 8192 the provider states
	engine.inputBudget = 40_000

	result := engine.Run(context.Background(), nil)

	if result.Reason != StopStop {
		t.Fatalf("reason = %q, want the run to recover and stop normally", result.Reason)
	}

	if *requests < 2 {
		t.Errorf("the request was not retried after the rejection (%d requests)", *requests)
	}

	if result.Budget.Recoveries != 1 {
		t.Errorf("continuations = %d, want the rejection to count as one", result.Budget.Recoveries)
	}

	// 85% of the stated 8192
	if engine.inputBudget != 6963 {
		t.Errorf("input budget = %d, want it narrowed to the stated window", engine.inputBudget)
	}
}

// Trimming happens on the wire, not in the history: every message the run
// started with is still in the conversation afterwards, and nothing has been
// summarised into its place.
func TestContextLimitNeverRewritesTheConversation(t *testing.T) {
	client, _ := contextLimitOnce(t)

	original := longConversation(40)

	engine, err := New(Options{Client: client, Messages: original})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	engine.inputBudget = 40_000

	result := engine.Run(context.Background(), nil)

	if len(result.Messages) < len(original) {
		t.Fatalf("the conversation shrank from %d to %d messages", len(original), len(result.Messages))
	}

	for index, message := range original {
		got := result.Messages[index]

		if got.Type != message.Type || got.Text != message.Text {
			t.Fatalf("message %d was rewritten: %q -> %q", index, message.Text, got.Text)
		}
	}
}

// Once the budget is down to what the instructions and tool schemas need there
// is nothing left to trim, and retrying would send the same request again.
func TestNarrowingStopsAtTheFloor(t *testing.T) {
	client, _ := contextLimitOnce(t)

	engine, err := New(Options{Client: client, Messages: longConversation(4)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	engine.inputBudget = MinInputTokens

	// no stated window, so the only move is stepping the budget down
	limit := llm.ContextLimit{}

	if engine.narrowInputBudget(limit, func(Event) {}) {
		t.Errorf("the budget narrowed to %d, below the %d floor", engine.inputBudget, MinInputTokens)
	}

	if engine.inputBudget != MinInputTokens {
		t.Errorf("input budget = %d, want it left at the floor", engine.inputBudget)
	}
}

// A rejection without a stated window steps the budget down by a quarter.
func TestNarrowingWithoutAStatedWindowStepsDown(t *testing.T) {
	client, _ := contextLimitOnce(t)

	engine, err := New(Options{Client: client})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	engine.inputBudget = 40_000

	if !engine.narrowInputBudget(llm.ContextLimit{}, func(Event) {}) {
		t.Fatal("expected the budget to narrow")
	}

	if engine.inputBudget != 30_000 {
		t.Errorf("input budget = %d, want 30000", engine.inputBudget)
	}
}

// A context limit that persists is eventually a real failure rather than an
// infinite retry loop.
func TestPersistentContextLimitGivesUp(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)

		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"message": "maximum context length exceeded"},
		})
	}))

	defer server.Close()

	client, err := llm.New(llm.Config{
		Provider: "custom",
		Model:    "test-model",
		APIKey:   "k",
		BaseURL:  server.URL,
	})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}

	engine, err := New(Options{
		Client:           client,
		Messages:         longConversation(40),
		MaxContinuations: 3,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	engine.inputBudget = 40_000

	result := engine.Run(context.Background(), nil)

	if result.Reason != StopError {
		t.Errorf("reason = %q, want error once narrowing stops helping", result.Reason)
	}

	if result.Err == nil {
		t.Error("the underlying provider error must be reported")
	}
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

		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"second time lucky"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))

	defer server.Close()

	client, err := llm.New(llm.Config{
		Provider: "custom",
		Model:    "test-model",
		APIKey:   "k",
		BaseURL:  server.URL,
	})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}

	engine, err := New(Options{
		Client:       client,
		Messages:     []Message{{Type: TypeUser, Text: "go"}},
		RetryBackoff: -1, // the retry itself is under test, not its pacing
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var retried bool

	result := engine.Run(context.Background(), func(event Event) {
		if event.Kind == EventRetry {
			retried = true
		}
	})

	if result.Reason != StopStop {
		t.Errorf("reason = %q, want the retry to succeed", result.Reason)
	}

	if !retried {
		t.Error("a retry must be visible to the caller")
	}
}

// A 4xx that is not a context limit is terminal: retrying a bad key or a missing
// model only burns the budget.
func TestNonRetriableErrorEndsTheRun(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)

		fmt.Fprint(w, `{"error":{"message":"invalid api key"}}`)
	}))

	defer server.Close()

	client, err := llm.New(llm.Config{
		Provider: "custom",
		Model:    "test-model",
		APIKey:   "k",
		BaseURL:  server.URL,
	})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}

	engine, err := New(Options{
		Client:   client,
		Messages: []Message{{Type: TypeUser, Text: "go"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result := engine.Run(context.Background(), nil)

	if result.Reason != StopError {
		t.Errorf("reason = %q, want error", result.Reason)
	}

	if result.Budget.Recoveries != 0 {
		t.Errorf("continuations = %d, want no retries for a credential problem", result.Budget.Recoveries)
	}
}

// When a provider rejects for length it states the real window. That is ground
// truth in a way the local catalogue is not, so the retry budgets against it
// rather than guessing again.
func TestContextLimitAdoptsTheProviderStatedWindow(t *testing.T) {
	requests := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++

		if requests == 1 {
			w.WriteHeader(http.StatusBadRequest)

			json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"message": "This model's maximum context length is 8192 tokens. However, your messages resulted in 40000 tokens.",
				},
			})

			return
		}

		w.Header().Set("Content-Type", "text/event-stream")

		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"fits now"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))

	defer server.Close()

	client, err := llm.New(llm.Config{
		Provider: "custom",
		Model:    "test-model",
		APIKey:   "k",
		BaseURL:  server.URL,
	})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}

	engine, err := New(Options{
		Client:   client,
		Messages: longConversation(40),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// the catalogue's guess is wildly optimistic for this model
	before := engine.inputBudget

	result := engine.Run(context.Background(), nil)

	if result.Reason != StopStop {
		t.Fatalf("reason = %q, want the run to recover", result.Reason)
	}

	// 85% of the stated 8192
	if engine.inputBudget != 6963 {
		t.Errorf("input budget = %d, want 6963 (85%% of the stated 8192); was %d",
			engine.inputBudget, before)
	}
}

// A rejection with no number still recovers, using the engine's own estimate.
func TestContextLimitWithoutANumberStillRecovers(t *testing.T) {
	requests := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++

		if requests == 1 {
			w.WriteHeader(http.StatusBadRequest)

			json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"message": "prompt is too long"},
			})

			return
		}

		w.Header().Set("Content-Type", "text/event-stream")

		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"ok"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))

	defer server.Close()

	client, _ := llm.New(llm.Config{
		Provider: "custom",
		Model:    "test-model",
		APIKey:   "k",
		BaseURL:  server.URL,
	})

	engine, err := New(Options{Client: client, Messages: longConversation(40)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	engine.inputBudget = 40_000

	if result := engine.Run(context.Background(), nil); result.Reason != StopStop {
		t.Errorf("reason = %q, want the run to recover", result.Reason)
	}
}
