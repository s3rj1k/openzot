package testutils

import (
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/outcome"
	"github.com/openzot/openzot/internal/provider"
)

// Turn is one reply of a fake model endpoint. Frames are answered as an event stream, and anything else is a plain
// status with an optional body, the way a failing provider answers.
type Turn struct {
	// Status is the HTTP status, zero for a stream.
	Status int

	// Header holds extra response headers.
	Header map[string]string

	// Frames are the data payloads of the event stream, closed by [DONE].
	Frames []string

	// Body is the body of a non-stream reply.
	Body string
}

// Frames is a turn that streams the given frames.
func Frames(frames ...string) Turn { return Turn{Frames: frames} }

// Reject is a turn that fails with a status and a body, which may be empty.
func Reject(status int, body string) Turn { return Turn{Status: status, Body: body} }

// WithHeader returns the turn with one more response header.
func (turn Turn) WithHeader(key, value string) Turn {
	turn.Header = maps.Clone(turn.Header)
	if turn.Header == nil {
		turn.Header = map[string]string{}
	}

	turn.Header[key] = value

	return turn
}

// SSE renders frames as an event stream, one blank line between events, closed by [DONE].
func SSE(frames ...string) string {
	var body strings.Builder

	for _, frame := range frames {
		body.WriteString("data: " + frame + "\n\n")
	}

	body.WriteString("data: [DONE]\n\n")

	return body.String()
}

// write answers one request with the turn.
func (turn Turn) write(w http.ResponseWriter) {
	for key, value := range turn.Header {
		w.Header().Set(key, value)
	}

	if turn.Status != 0 {
		w.WriteHeader(turn.Status)

		_, _ = io.WriteString(w, turn.Body)

		return
	}

	w.Header().Set("Content-Type", "text/event-stream")

	_, _ = io.WriteString(w, SSE(turn.Frames...))
}

// Call is one request an endpoint received.
type Call struct {
	// Header holds the request headers.
	Header http.Header

	// Body is the request body.
	Body string
}

// Server is a fake model endpoint on the OpenAI-compatible wire format.
type Server struct {
	// URL is the base URL a client or a config points at.
	URL string

	mu    sync.Mutex
	calls []Call
}

// NewServer starts an endpoint that answers each request with what reply returns for it. The request number starts at 1
// and the body is what the client sent. It is closed when the test ends.
func NewServer(t *testing.T, reply func(request int, body string) Turn) *Server {
	t.Helper()

	s := &Server{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		s.mu.Lock()
		s.calls = append(s.calls, Call{Header: r.Header.Clone(), Body: string(body)})
		request := len(s.calls)
		s.mu.Unlock()

		reply(request, string(body)).write(w)
	}))

	t.Cleanup(server.Close)

	s.URL = server.URL

	return s
}

// Script starts an endpoint that answers the nth request with the nth turn, and every later one with the last.
func Script(t *testing.T, turns ...Turn) *Server {
	t.Helper()

	return NewServer(t, func(request int, _ string) Turn {
		return turns[min(request, len(turns))-1]
	})
}

// Raw starts an endpoint served by the handler, for a test that needs to control the wire itself. It records nothing.
func Raw(t *testing.T, handler http.Handler) *Server {
	t.Helper()

	server := httptest.NewServer(handler)

	t.Cleanup(server.Close)

	return &Server{URL: server.URL}
}

// Requests is how many requests the endpoint has answered.
func (s *Server) Requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.calls)
}

// Calls are the requests the endpoint has received, oldest first.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]Call(nil), s.calls...)
}

// Bodies are the request bodies the endpoint has received, oldest first.
func (s *Server) Bodies() []string {
	calls := s.Calls()
	bodies := make([]string, len(calls))

	for i, call := range calls {
		bodies[i] = call.Body
	}

	return bodies
}

// Client is a provider client pointed at the endpoint. The tweaks change its config before it is built.
func (s *Server) Client(t *testing.T, tweak ...func(*provider.ClientConfig)) *provider.Client {
	t.Helper()

	config := provider.ClientConfig{Provider: "test", Model: "test-model", APIKey: "test-key", BaseURL: s.URL}

	for _, change := range tweak {
		change(&config)
	}

	client, err := provider.NewClient(t.Context(), config)
	require.NoError(t, err)

	return client
}

// Model is the endpoint's language model, for a test that runs the engine on it.
func (s *Server) Model(t *testing.T, tweak ...func(*provider.ClientConfig)) fantasy.LanguageModel {
	t.Helper()

	return s.Client(t, tweak...).Model()
}

// Text is a frame that streams some content.
func Text(s string) string {
	return fmt.Sprintf(`{"choices":[{"delta":{"content":%q}}]}`, s)
}

// Stop is the frame that ends a turn normally.
func Stop() string {
	return `{"choices":[{"delta":{},"finish_reason":"stop"}]}`
}

// Truncated is the frame that ends a turn cut off by the length limit.
func Truncated() string {
	return `{"choices":[{"delta":{},"finish_reason":"length"}]}`
}

// Usage is a final frame carrying the provider's own token counts.
func Usage(prompt, completion int) string {
	return fmt.Sprintf(
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
		prompt, completion, prompt+completion,
	)
}

// Tool is a frame asking for one tool call.
func Tool(id, name, arguments string) string {
	return fmt.Sprintf(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":"tool_calls"}]}`,
		id, name, arguments,
	)
}

// ToolCalls is a frame asking for several tools at once, finishing with the given reason. Each call is an id, a name and
// its arguments.
func ToolCalls(finish string, calls ...[3]string) string {
	parts := make([]string, len(calls))

	for i, call := range calls {
		parts[i] = fmt.Sprintf(
			`{"index":%d,"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}`,
			i, call[0], call[1], call[2])
	}

	return `{"choices":[{"delta":{"tool_calls":[` + strings.Join(parts, ",") +
		`]},"finish_reason":` + fmt.Sprintf("%q", finish) + `}]}`
}

// Settle is a frame in which the model calls the success tool, which ends the run.
func Settle(summary string) string {
	return Tool("done", outcome.SuccessTool, fmt.Sprintf(`{"summary":%q}`, summary))
}

// ScriptedModel is a language model whose nth request is answered with the nth set of frames, and every later request
// with the last set.
func ScriptedModel(t *testing.T, turns ...[]string) fantasy.LanguageModel {
	t.Helper()

	replies := make([]Turn, len(turns))

	for i, frames := range turns {
		replies[i] = Frames(frames...)
	}

	return Script(t, replies...).Model(t)
}

// Serve starts an endpoint served by the handler and returns a client pointed at it. The tweaks change the client's
// config before it is built.
func Serve(t *testing.T, handler http.HandlerFunc, tweak ...func(*provider.ClientConfig)) *provider.Client {
	t.Helper()

	return Raw(t, handler).Client(t, tweak...)
}

// Canned is a client whose endpoint answers every request with the same event stream of frames.
func Canned(t *testing.T, frames ...string) *provider.Client {
	t.Helper()

	return Script(t, Frames(frames...)).Client(t)
}
