package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
)

// wireRequest is what a fake endpoint saw.
type wireRequest struct {
	mu      sync.Mutex
	path    string
	headers http.Header
	body    map[string]any
}

// capture is a handler that records the request and answers with a finished turn.
func (r *wireRequest) capture(w http.ResponseWriter, req *http.Request) {
	raw, _ := io.ReadAll(req.Body)

	r.mu.Lock()

	r.path = req.URL.Path
	r.headers = req.Header.Clone()
	r.body = map[string]any{}

	json.Unmarshal(raw, &r.body)

	r.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")

	fmt.Fprint(w, sse(`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`))
}

func (r *wireRequest) messages() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()

	var messages []map[string]any

	list, _ := r.body["messages"].([]any)

	for _, entry := range list {
		messages = append(messages, entry.(map[string]any))
	}

	return messages
}

func TestStreamAssemblesText(t *testing.T) {
	result := collect(frames(t,
		`{"choices":[{"delta":{"content":"Hel"}}]}`,
		`{"choices":[{"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
	), hello())

	if result.err != nil {
		t.Fatalf("stream: %v", result.err)
	}

	if result.text != "Hello" || result.finish != fantasy.FinishReasonStop {
		t.Errorf("text = %q, finish = %q", result.text, result.finish)
	}
}

func TestStreamAssemblesFragmentedParallelToolCalls(t *testing.T) {
	result := collect(frames(t,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"shell","arguments":"{\"cmd\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","type":"function","function":{"name":"read","arguments":"{\"path\":\"x\"}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"pwd\"}"}}]},"finish_reason":"tool_calls"}]}`,
	), hello())

	if result.err != nil {
		t.Fatalf("stream: %v", result.err)
	}

	if len(result.calls) != 2 {
		t.Fatalf("calls = %+v, want two", result.calls)
	}

	first, second := result.calls[0], result.calls[1]

	if first.ToolCallID != "a" || first.ToolName != "shell" || first.Input != `{"cmd":"pwd"}` {
		t.Errorf("first call = %+v, want its argument fragments joined", first)
	}

	if second.ToolCallID != "b" || second.ToolName != "read" {
		t.Errorf("second call = %+v", second)
	}
}

// A model calling a tool with no parameters often sends "" for the arguments.
func TestAToolCallWithEmptyArgumentsIsAnEmptyObject(t *testing.T) {
	result := collect(frames(t,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"list","arguments":""}}]},"finish_reason":"tool_calls"}]}`,
	), hello())

	if len(result.calls) != 1 {
		t.Fatalf("calls = %+v", result.calls)
	}

	if input := result.calls[0].Input; input != "" && input != "{}" {
		t.Errorf("input = %q, want empty or an empty object", input)
	}
}

// A turn cut off at the output limit mid tool call must not dispatch the half
// call, and must say it was cut off so the loop can ask for the rest.
func TestATruncatedTurnReportsLengthAndDropsTheHalfCall(t *testing.T) {
	result := collect(frames(t,
		`{"choices":[{"delta":{"content":"partial"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"shell","arguments":"{\"cmd\":\"l"}}]},"finish_reason":"length"}]}`,
	), hello())

	if result.finish != fantasy.FinishReasonLength {
		t.Errorf("finish = %q, want length", result.finish)
	}

	if len(result.calls) != 0 {
		t.Errorf("calls = %+v, want the incomplete call withheld", result.calls)
	}
}

func TestStreamAcceptsBothReasoningFields(t *testing.T) {
	for field, want := range map[string]string{"reasoning_content": "thought one", "reasoning": "thought two"} {
		result := collect(frames(t,
			fmt.Sprintf(`{"choices":[{"delta":{%q:%q}}]}`, field, want),
			`{"choices":[{"delta":{"content":"answer"},"finish_reason":"stop"}]}`,
		), hello())

		if result.reasoning != want || result.text != "answer" {
			t.Errorf("%s: reasoning = %q, text = %q, want %q kept apart from the answer", field, result.reasoning, result.text, want)
		}
	}
}

func TestStreamCapturesUsage(t *testing.T) {
	// the shape real servers send: a trailing chunk with no choices
	trailing := collect(frames(t,
		`{"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}}`,
	), hello())

	if trailing.usage.InputTokens != 10 || trailing.usage.OutputTokens != 3 {
		t.Errorf("usage = %+v, want the reported counts", trailing.usage)
	}

	// a server may leave the total out, and the counts are still real
	noTotal := collect(frames(t,
		`{"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1234,"completion_tokens":56}}`,
	), hello())

	if noTotal.usage.InputTokens != 1234 || noTotal.usage.OutputTokens != 56 {
		t.Errorf("usage = %+v, want counts read without a total", noTotal.usage)
	}
}

func TestStreamSurfacesAnInBandErrorAsRetriable(t *testing.T) {
	result := collect(frames(t, `{"error":{"message":"upstream exploded","type":"server_error"}}`), hello())

	if result.err == nil {
		t.Fatal("an error frame must surface")
	}

	if !IsProviderError(result.err) || !IsRetriable(result.err) {
		t.Errorf("err = %v, want a retriable provider error", result.err)
	}
}

func TestAStreamThatEndsUnfinishedIsRetriable(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"content":"cut"}}]}`+"\n\n")
	})

	result := collect(client, hello())

	if result.err == nil || !IsRetriable(result.err) {
		t.Errorf("err = %v, want a retriable failure for a stream with no ending", result.err)
	}
}

// A connection cut at each point of a turn - after a frame, before any response,
// by a reset - is an outage the run should wait out, not one that ends it. These
// are the real transport errors, so they prove the rules recognise them by type.
func TestACutConnectionIsRetriable(t *testing.T) {
	tests := map[string]http.HandlerFunc{
		"cut after a frame": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")

			fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"content":"cut"}}]}`+"\n\n")
			w.(http.Flusher).Flush()

			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		},
		"closed before any response": func(w http.ResponseWriter, _ *http.Request) {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		},
		"reset before any response": func(w http.ResponseWriter, _ *http.Request) {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.(*net.TCPConn).SetLinger(0)
			conn.Close()
		},
	}

	for name, handler := range tests {
		t.Run(name, func(t *testing.T) {
			err := collect(serve(t, handler), hello()).err
			if err == nil {
				t.Fatal("a cut connection must surface as an error")
			}

			if !IsRetriable(err) {
				t.Errorf("err = %v, want a retriable failure", err)
			}
		})
	}
}

func TestStreamClassifiesHTTPErrors(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		header    string
		body      string
		retriable bool
		limited   bool
	}{
		{"server error", 500, "", `{"error":{"message":"oops"}}`, true, false},
		{"rate limit with advice", 429, "7", `{"error":{"message":"slow down"}}`, false, true},
		{"bad key", 401, "", `{"error":{"message":"bad key"}}`, false, false},
		{"not json", 502, "", `bad gateway`, true, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := serve(t, func(w http.ResponseWriter, _ *http.Request) {
				if test.header != "" {
					w.Header().Set("Retry-After", test.header)
				}

				w.WriteHeader(test.status)
				io.WriteString(w, test.body)
			})

			err := collect(client, hello()).err
			if err == nil {
				t.Fatal("expected an error")
			}

			if IsRetriable(err) != test.retriable || IsRateLimited(err) != test.limited {
				t.Errorf("retriable = %v, limited = %v, want %v, %v", IsRetriable(err), IsRateLimited(err), test.retriable, test.limited)
			}

			if failure := FailureOf(err); failure == nil || failure.Status != test.status {
				t.Errorf("failure = %+v, want the status kept as evidence", failure)
			}

			if test.limited {
				if delay, ok := RetryAfter(err); !ok || delay != 7*time.Second {
					t.Errorf("Retry-After = %v, %v, want 7s", delay, ok)
				}
			}
		})
	}
}

func TestAContextOverflowIsRecognisedFromTheWire(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"This model's maximum context length is 8192 tokens. However, your messages resulted in 9000 tokens.","code":"context_length_exceeded"}}`)
	})

	limit, ok := DetectContextLimit(collect(client, hello()).err)
	if !ok || limit.MaxTokens != 8192 {
		t.Errorf("limit = %+v, %v, want the stated window", limit, ok)
	}
}

// The shape of everything sent: the credential and model, the endpoint, the
// limit under the field every OpenAI-compatible server reads, and the tools.
func TestStreamSendsTheRequestAsConfigured(t *testing.T) {
	var seen wireRequest

	client := serve(t, seen.capture)

	limit := int64(321)

	call := fantasy.Call{
		Prompt:          fantasy.Prompt{fantasy.NewSystemMessage("be brief"), fantasy.NewUserMessage("hi")},
		MaxOutputTokens: &limit,
		Tools: []fantasy.Tool{fantasy.FunctionTool{
			Name: "shell", Description: "run", InputSchema: map[string]any{"type": "object"},
		}},
	}

	if result := collect(client, call); result.err != nil {
		t.Fatalf("stream: %v", result.err)
	}

	if seen.path != "/chat/completions" {
		t.Errorf("path = %q", seen.path)
	}

	if got := seen.headers.Get("Authorization"); got != "Bearer test-key" {
		t.Errorf("Authorization = %q", got)
	}

	if seen.body["model"] != "test-model" || seen.body["stream"] != true {
		t.Errorf("model = %v, stream = %v", seen.body["model"], seen.body["stream"])
	}

	if seen.body["max_tokens"] != float64(321) {
		t.Errorf("max_tokens = %v, want the limit under the field servers read", seen.body["max_tokens"])
	}

	if _, present := seen.body["max_completion_tokens"]; present {
		t.Error("max_completion_tokens must not be sent alongside")
	}

	tools, _ := seen.body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v, want the one offered", seen.body["tools"])
	}
}

// fantasy sends max_completion_tokens for a model whose name looks like a hosted
// reasoning model, and a local server may be serving one under such a name. The
// field every OpenAI-compatible server reads is max_tokens, so that is the one
// that goes out, whatever the model is called.
func TestTheLimitIsMaxTokensEvenForAReasoningModelName(t *testing.T) {
	var seen wireRequest

	client := serve(t, seen.capture, func(c *ClientConfig) { c.Model = "gpt-5.4" })

	limit := int64(64)

	collect(client, fantasy.Call{Prompt: hello().Prompt, MaxOutputTokens: &limit})

	if seen.body["max_tokens"] != float64(64) {
		t.Errorf("max_tokens = %v, want the limit", seen.body["max_tokens"])
	}

	if _, present := seen.body["max_completion_tokens"]; present {
		t.Error("max_completion_tokens leaked through for a reasoning-looking model name")
	}
}

func TestStreamOmitsTheLimitWhenUnset(t *testing.T) {
	var seen wireRequest

	collect(serve(t, seen.capture), hello())

	for _, field := range []string{"max_tokens", "max_completion_tokens"} {
		if _, present := seen.body[field]; present {
			t.Errorf("%s was sent with no limit set", field)
		}
	}
}

// A model name that a hosted provider also uses must not change the wire
// format: this is a chat-completions endpoint whatever the name says.
func TestAHostedModelNameDoesNotSelectAnotherWireFormat(t *testing.T) {
	var seen wireRequest

	collect(serve(t, seen.capture, func(c *ClientConfig) { c.Model = "gpt-5.4" }), hello())

	if seen.path != "/chat/completions" {
		t.Errorf("path = %q, want chat-completions for any model name", seen.path)
	}
}

// A tool that produced no output still answers its call, and the message
// carries its content: a server that validates the shape rejects one without.
func TestAnEmptyToolResultStillCarriesContent(t *testing.T) {
	prompt := fantasy.Prompt{
		fantasy.NewUserMessage("go"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ToolCallPart{ToolCallID: "c1", ToolName: "shell", Input: `{}`},
		}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{ToolCallID: "c1", Output: fantasy.ToolResultOutputContentText{Text: ""}},
		}},
	}

	for _, contentArray := range []bool{false, true} {
		var seen wireRequest

		client := serve(t, seen.capture, func(c *ClientConfig) { c.ContentArray = contentArray })

		collect(client, fantasy.Call{Prompt: prompt})

		messages := seen.messages()

		tool := messages[len(messages)-1]

		content, present := tool["content"]
		if !present {
			t.Fatalf("contentArray=%v: the tool message has no content key: %v", contentArray, tool)
		}

		if contentArray {
			if parts, ok := content.([]any); !ok || len(parts) != 0 {
				t.Errorf("content = %v, want an empty array", content)
			}
		} else if content != "" {
			t.Errorf("content = %v, want an empty string", content)
		}
	}
}

// Off by default: every endpoint zot reaches out of the box takes the string.
func TestContentIsAStringUnlessAnArrayIsAskedFor(t *testing.T) {
	var seen wireRequest

	collect(serve(t, seen.capture), fantasy.Call{Prompt: fantasy.Prompt{
		fantasy.NewSystemMessage("be brief"), fantasy.NewUserMessage("hi"),
	}})

	for _, message := range seen.messages() {
		if _, isString := message["content"].(string); !isString {
			t.Errorf("content = %v, want a plain string by default", message["content"])
		}
	}
}

func TestContentArrayWrapsEveryMessageInParts(t *testing.T) {
	var seen wireRequest

	client := serve(t, seen.capture, func(c *ClientConfig) { c.ContentArray = true })

	collect(client, fantasy.Call{Prompt: fantasy.Prompt{
		fantasy.NewSystemMessage("be brief"), fantasy.NewUserMessage("hi"),
	}})

	messages := seen.messages()
	if len(messages) != 2 {
		t.Fatalf("messages = %v", messages)
	}

	for index, want := range []string{"be brief", "hi"} {
		parts, ok := messages[index]["content"].([]any)
		if !ok || len(parts) != 1 {
			t.Fatalf("message %d content = %v, want one part", index, messages[index]["content"])
		}

		part := parts[0].(map[string]any)

		if part["type"] != "text" || part["text"] != want {
			t.Errorf("message %d part = %v", index, part)
		}
	}
}

func TestNoAmbientCredentialReachesTheWire(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-from-the-environment")
	t.Setenv("OPENAI_ORG_ID", "org-ambient")
	t.Setenv("OPENAI_PROJECT_ID", "proj-ambient")

	var seen wireRequest

	client := serve(t, seen.capture, func(c *ClientConfig) { c.APIKey = "" })

	// loopback, so no key is required, and none may be invented
	if err := collect(client, hello()).err; err != nil {
		t.Fatalf("stream: %v", err)
	}

	for _, header := range []string{"Authorization", "OpenAI-Organization", "OpenAI-Project"} {
		if value := seen.headers.Get(header); value != "" {
			t.Errorf("%s = %q, want nothing the operator did not configure", header, value)
		}
	}
}

func TestAConfiguredKeyIsNotReplacedByTheEnvironment(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-from-the-environment")

	var seen wireRequest

	collect(serve(t, seen.capture), hello())

	if got := seen.headers.Get("Authorization"); got != "Bearer test-key" {
		t.Errorf("Authorization = %q, want the configured key", got)
	}
}

func TestClientExposesItsResolvedConfig(t *testing.T) {
	client := serve(t, func(http.ResponseWriter, *http.Request) {}, func(c *ClientConfig) { c.BaseURL += "/" })

	config := client.Config()

	if config.Model != "test-model" || strings.HasSuffix(config.BaseURL, "/") {
		t.Errorf("config = %+v", config)
	}
}

func TestNewRefusesAnInvalidConfig(t *testing.T) {
	if _, err := NewClient(ClientConfig{}); err == nil {
		t.Error("an empty config must not connect")
	}
}

func TestStreamCancellationStopsTheTurn(t *testing.T) {
	release := make(chan struct{})

	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"content":"x"}}]}`+"\n\n")

		w.(http.Flusher).Flush()

		select {
		case <-release:
		case <-r.Context().Done():
		}
	})

	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})

	go func() {
		defer close(done)

		for range client.Stream(ctx, hello()) {
		}
	}()

	time.Sleep(50 * time.Millisecond)

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the context did not end the stream")
	}
}

// Walking away from the stream, as the runaway guard does, must release the
// connection rather than leave the response body open.
func TestAnAbandonedStreamReleasesItsConnection(t *testing.T) {
	released := make(chan struct{})

	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		for {
			if _, err := fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"content":"x"}}]}`+"\n\n"); err != nil {
				close(released)

				return
			}

			w.(http.Flusher).Flush()

			select {
			case <-r.Context().Done():
				close(released)

				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	})

	ctx, cancel := context.WithCancel(context.Background())

	for part := range client.Stream(ctx, hello()) {
		if part.Type == fantasy.StreamPartTypeTextDelta {
			break
		}
	}

	cancel()

	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("the server never saw the connection go away")
	}
}

// A long turn is not a hung one: only silence is bounded, so a stream that keeps
// producing must not be cut off however long it runs.
func TestASlowButProgressingStreamIsNotCutOff(t *testing.T) {
	withStallTimeout(t, 300*time.Millisecond)

	client := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		for i := 0; i < 20; i++ {
			fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"content":"x"}}]}`+"\n\n")

			w.(http.Flusher).Flush()

			time.Sleep(50 * time.Millisecond)
		}

		fmt.Fprint(w, sse(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`))
	})

	result := collect(client, hello())

	if result.err != nil {
		t.Fatalf("a stream that kept producing was cut off: %v", result.err)
	}

	if result.text != strings.Repeat("x", 20) {
		t.Errorf("text = %q, want all 20 tokens", result.text)
	}
}

// What is pathological is a stream that goes quiet and stays quiet. It has to
// fail, and to fail retriably.
func TestAStalledStreamFailsRetriably(t *testing.T) {
	withStallTimeout(t, 100*time.Millisecond)

	hang := make(chan struct{})

	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"content":"x"}}]}`+"\n\n")

		w.(http.Flusher).Flush()

		select {
		case <-hang:
		case <-r.Context().Done():
		}
	})

	t.Cleanup(func() { close(hang) })

	done := make(chan turn, 1)

	go func() { done <- collect(client, hello()) }()

	select {
	case result := <-done:
		if result.err == nil {
			t.Fatal("a stream that went silent must not hang forever")
		}

		if !IsRetriable(result.err) {
			t.Errorf("a stalled stream should be retriable: %v", result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stream that went silent was never cut off")
	}
}

// The silence bound covers the error path too: a server that sends a status and
// then holds the body open must not park the run.
func TestAnErrorResponseWithAStalledBodyDoesNotWedge(t *testing.T) {
	withStallTimeout(t, 100*time.Millisecond)

	hang := make(chan struct{})

	client := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)

		w.(http.Flusher).Flush()

		select {
		case <-hang:
		case <-r.Context().Done():
		}
	})

	t.Cleanup(func() { close(hang) })

	done := make(chan error, 1)

	go func() { done <- collect(client, hello()).err }()

	select {
	case err := <-done:
		if err == nil {
			t.Error("a refused request must report an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an error response held open wedged the turn")
	}
}
