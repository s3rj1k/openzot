package loop

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openzot/openzot/internal/provider"
)

// A gateway's reasoning_details come out of one turn's stream and go back on the assistant
// message that carries that turn's calls - the whole loop away and back. A
// reasoning model whose thinking is dropped between tool rounds degrades, and
// some upstreams reject the request outright once the chain has grown.
func TestReasoningDetailsSurviveTheToolRound(t *testing.T) {
	var replayed string

	turn := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		if strings.Contains(string(body), "reasoning_details") {
			replayed = string(body)
		}

		w.Header().Set("Content-Type", "text/event-stream")

		if turn == 0 {
			turn++

			for _, frame := range []string{
				`{"choices":[{"delta":{"reasoning_details":[{"type":"reasoning.text","index":0,"text":"plan the read","signature":"sig-1"}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"probe","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
			} {
				fmt.Fprintf(w, "data: %s\n\n", frame)
			}
		} else {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`+"\n\n")
		}

		fmt.Fprint(w, "data: [DONE]\n\n")
	}))

	t.Cleanup(server.Close)

	client, err := provider.New(provider.Config{Provider: "custom", Model: "m", APIKey: "k", BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}

	engine, err := New(Options{
		Client:   client,
		Messages: []Message{{Type: TypeUser, Text: "go"}},
		Tools: map[string]ToolDefinition{"probe": {
			Name:       "probe",
			Parameters: map[string]any{"type": "object"},
			Handler:    func(context.Context, map[string]any) (any, error) { return "ok", nil },
		}},
		MaxIterations: 4,
	})
	if err != nil {
		t.Fatal(err)
	}

	result := engine.Run(context.Background(), nil)

	if result.Reason != StopStop {
		t.Fatalf("reason = %q (%s)", result.Reason, result.Message)
	}

	for _, want := range []string{"plan the read", "sig-1"} {
		if !strings.Contains(replayed, want) {
			t.Errorf("the follow-up request is missing %q:\n%s", want, replayed)
		}
	}
}
