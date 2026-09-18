package provider

import (
	"context"
	"net/http"
	"slices"
	"testing"
)

// The point of the Transport seam is that a wire format that looks nothing like
// OpenAI's can be added without touching the engine, the client, or the config.
// These tests exercise that claim directly rather than describing it.

// fakeTransport stands in for a future native provider - Anthropic Messages,
// Bedrock Converse - by implementing the interface and nothing else.
type fakeTransport struct {
	name   string
	seen   Config
	events []Event
}

func (f *fakeTransport) Name() string { return f.name }

func (f *fakeTransport) Stream(_ context.Context, config Config, _ Request) <-chan Event {
	f.seen = config

	events := make(chan Event, len(f.events))

	for _, event := range f.events {
		events <- event
	}

	close(events)

	return events
}

func TestChatCompletionsIsRegistered(t *testing.T) {
	registered := Transports()

	if !slices.Contains(registered, TransportChatCompletions) {
		t.Errorf("transport %q is not registered; have %v", TransportChatCompletions, registered)
	}
}

func TestTransportsAreSorted(t *testing.T) {
	registered := Transports()

	if !slices.IsSorted(registered) {
		t.Errorf("Transports() is not sorted: %v", registered)
	}
}

// A duplicate registration is a programming error, and one that would otherwise
// silently shadow a working transport.
func TestDuplicateRegistrationPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("registering a transport twice must panic")
		}
	}()

	RegisterTransport(TransportChatCompletions, func(*http.Client) Transport { return nil })
}

// The whole point: a new wire format is one registration, with no change to the
// client, the config or the engine.
func TestACustomTransportNeedsNothingElse(t *testing.T) {
	fake := &fakeTransport{
		name: "test-only-messages",
		events: []Event{
			{Token: "hello"},
			{FinishReason: FinishStop},
		},
	}

	RegisterTransport(fake.name, func(*http.Client) Transport { return fake })

	t.Cleanup(func() {
		transportsMu.Lock()

		delete(transports, fake.name)

		transportsMu.Unlock()
	})

	resolved, err := Config{Provider: "custom", Model: "m", APIKey: "k", BaseURL: "https://x.example.com/v1"}.Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	transport, err := lookupTransport(fake.name, &http.Client{})
	if err != nil {
		t.Fatalf("lookupTransport: %v", err)
	}

	var text string

	for event := range transport.Stream(context.Background(), resolved, Request{}) {
		text += event.Token
	}

	if text != "hello" {
		t.Errorf("text = %q", text)
	}

	// the transport receives the resolved configuration, so it has the endpoint
	// and credential it needs without reaching back into the client
	if fake.seen.Model != "m" || fake.seen.APIKey != "k" {
		t.Errorf("transport saw %+v, want the resolved config", fake.seen)
	}
}

func TestLookupUnknownTransport(t *testing.T) {
	_, err := lookupTransport("nope", &http.Client{})

	if err == nil {
		t.Fatal("an unknown transport must be reported")
	}

	// the error lists what is available, because the caller is usually a typo
	if !slices.Contains(Transports(), TransportChatCompletions) {
		t.Error("the registry should still be intact")
	}
}

// The client picks its transport and reports which one it got, which is what
// the UI header shows.
func TestClientUsesChatCompletions(t *testing.T) {
	client, err := New(Config{Provider: "gw", Model: "gpt-5.4-mini", APIKey: "k", BaseURL: "https://gw.example.com/v1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if got := client.Transport(); got != TransportChatCompletions {
		t.Errorf("Transport = %q, want %q", got, TransportChatCompletions)
	}
}
