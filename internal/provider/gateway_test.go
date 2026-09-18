package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/openzot/openzot/internal/catalogue"
)

// Gateways route by a provider-qualified model name - "openai/gpt-4o",
// "z-ai/glm-5.2" - so the prefix is not decoration, it is the routing. zot never
// adds one: the name the operator gives is the name sent. These tests pin what
// has to be true for a prefixed name to work: it reaches the wire unchanged, and
// it still resolves its real context window.

// captureModel serves one successful turn and records the "model" field of the
// request body the client sent.
func captureModel(t *testing.T, config Config) string {
	t.Helper()

	seen := make(chan string, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		var payload struct {
			Model string `json:"model"`
		}

		_ = json.Unmarshal(body, &payload)

		select {
		case seen <- payload.Model:
		default:
		}

		w.Header().Set("Content-Type", "text/event-stream")

		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))

	t.Cleanup(server.Close)

	config.BaseURL = server.URL

	client, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for event := range client.Stream(context.Background(), Request{}) {
		if event.Err != nil {
			t.Fatalf("stream: %v", event.Err)
		}
	}

	select {
	case model := <-seen:
		return model
	default:
		t.Fatal("the provider was never called")

		return ""
	}
}

// The prefix is the routing, so it has to arrive intact. Stripping it would send
// the gateway a model it cannot resolve.
func TestAGatewayIsSentThePrefixedModelVerbatim(t *testing.T) {
	for _, model := range []string{
		"openai/gpt-4o",
		"anthropic/claude-5-sonnet",
		"z-ai/glm-5.2",
		"meta-llama/llama-4-70b-instruct",
	} {
		t.Run(model, func(t *testing.T) {
			got := captureModel(t, Config{
				Provider: "gateway",
				Model:    model,
				APIKey:   "sk-test",
			})

			if got != model {
				t.Errorf("provider received model %q, want %q sent unchanged", got, model)
			}
		})
	}
}

// The prefixed name still has to resolve its real limits. A budget decision made
// against the conservative default window - because the lookup could not see
// past the prefix - would trim far too early on a large-context model.
func TestAPrefixedModelResolvesTheRealContextWindow(t *testing.T) {
	tests := []struct {
		model      string
		wantWindow int
	}{
		{model: "z-ai/glm-5.2", wantWindow: 1_000_000},
		{model: "openai/gpt-5.4-mini", wantWindow: 400_000},
		{model: "openai/gpt-4o", wantWindow: 128_000},
	}

	for _, test := range tests {
		t.Run(test.model, func(t *testing.T) {
			config, err := Config{
				Provider: "gateway",
				Model:    test.model,
				BaseURL:  "https://gateway.example.com/v1",
				APIKey:   "sk-test",
			}.Resolve()
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}

			if got := catalogue.InputBudget(config.Model); got <= 0 {
				t.Fatalf("input budget for %q is %d", test.model, got)
			}

			// the budget is a fraction of the window, so a prefixed name that
			// fell through to the default (128k) window would be capped far
			// below a large-context model's real budget
			if test.wantWindow > 200_000 && catalogue.InputBudget(config.Model) <= 128_000 {
				t.Errorf("%q resolved to a small budget %d - the prefix was not stripped for the lookup",
					test.model, catalogue.InputBudget(config.Model))
			}
		})
	}
}
