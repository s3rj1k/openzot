package provider_test

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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/provider"
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

	_ = json.Unmarshal(raw, &r.body)

	r.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")

	fmt.Fprint(w, sse(`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`))
}

func (r *wireRequest) messages() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()

	list, _ := r.body["messages"].([]any)

	messages := make([]map[string]any, 0, len(list))

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

	require.NoError(t, result.err)

	assert.Equal(t, "Hello", result.text)
	assert.Equal(t, fantasy.FinishReasonStop, result.finish)
}

func TestStreamAssemblesFragmentedParallelToolCalls(t *testing.T) {
	result := collect(frames(t,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"shell","arguments":"{\"cmd\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","type":"function","function":{"name":"read","arguments":"{\"path\":\"x\"}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"pwd\"}"}}]},"finish_reason":"tool_calls"}]}`,
	), hello())

	require.NoError(t, result.err)

	require.Len(t, result.calls, 2)

	first, second := result.calls[0], result.calls[1]

	assert.Equal(t, "a", first.ToolCallID, "want its argument fragments joined")
	assert.Equal(t, litShell, first.ToolName, "want its argument fragments joined")
	assert.JSONEq(t, `{"cmd":"pwd"}`, first.Input, "want its argument fragments joined")

	assert.Equal(t, "b", second.ToolCallID)
	assert.Equal(t, "read", second.ToolName)
}

// A model calling a tool with no parameters often sends "" for the arguments.
func TestAToolCallWithEmptyArgumentsIsAnEmptyObject(t *testing.T) {
	result := collect(frames(t,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"list","arguments":""}}]},"finish_reason":"tool_calls"}]}`,
	), hello())

	require.Len(t, result.calls, 1)

	assert.Contains(t, []string{"", "{}"}, result.calls[0].Input, "want empty or an empty object")
}

// A turn cut off at the output limit mid tool call must not dispatch the half
// call, and must say it was cut off so the loop can ask for the rest.
func TestATruncatedTurnReportsLengthAndDropsTheHalfCall(t *testing.T) {
	result := collect(frames(t,
		`{"choices":[{"delta":{"content":"partial"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"shell","arguments":"{\"cmd\":\"l"}}]},"finish_reason":"length"}]}`,
	), hello())

	assert.Equal(t, fantasy.FinishReasonLength, result.finish)

	assert.Empty(t, result.calls, "want the incomplete call withheld")
}

func TestStreamAcceptsBothReasoningFields(t *testing.T) {
	for field, want := range map[string]string{"reasoning_content": "thought one", "reasoning": "thought two"} {
		result := collect(frames(t,
			fmt.Sprintf(`{"choices":[{"delta":{%q:%q}}]}`, field, want),
			`{"choices":[{"delta":{"content":"answer"},"finish_reason":"stop"}]}`,
		), hello())

		assert.Equal(t, want, result.reasoning, "%s: reasoning = %q, text = %q, want %q kept apart from the answer", field, result.reasoning, result.text, want)
		assert.Equal(t, "answer", result.text, "%s: reasoning = %q, text = %q, want %q kept apart from the answer", field, result.reasoning, result.text, want)
	}
}

func TestStreamCapturesUsage(t *testing.T) {
	// the shape real servers send. A trailing chunk with no choices
	trailing := collect(frames(t,
		`{"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}}`,
	), hello())

	assert.EqualValues(t, 10, trailing.usage.InputTokens)
	assert.EqualValues(t, 3, trailing.usage.OutputTokens)

	// a server may leave the total out, and the counts are still real
	noTotal := collect(frames(t,
		`{"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1234,"completion_tokens":56}}`,
	), hello())

	assert.EqualValues(t, 1234, noTotal.usage.InputTokens, "want counts read without a total")
	assert.EqualValues(t, 56, noTotal.usage.OutputTokens, "want counts read without a total")
}

func TestStreamSurfacesAnInBandErrorAsRetriable(t *testing.T) {
	result := collect(frames(t, `{"error":{"message":"upstream exploded","type":"server_error"}}`), hello())

	require.Error(t, result.err)

	assert.True(t, provider.IsProviderError(result.err))
	assert.True(t, provider.IsRetriable(result.err))
}

func TestAStreamThatEndsUnfinishedIsRetriable(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"content":"cut"}}]}`+"\n\n")
	})

	result := collect(client, hello())

	require.Error(t, result.err, "want a retriable failure for a stream with no ending")
	assert.True(t, provider.IsRetriable(result.err), "want a retriable failure for a stream with no ending")
}

// A connection cut at each point of a turn - after a frame, before any response,
// by a reset - is an outage the run should wait out, not one that ends it. These
// are the real transport errors, so they prove the rules recognize them by type.
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
			_ = conn.(*net.TCPConn).SetLinger(0)
			conn.Close()
		},
	}

	for name, handler := range tests {
		t.Run(name, func(t *testing.T) {
			err := collect(serve(t, handler), hello()).err
			require.Error(t, err, "a cut connection must surface as an error")

			assert.True(t, provider.IsRetriable(err))
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
				_, _ = io.WriteString(w, test.body)
			})

			err := collect(client, hello()).err
			require.Error(t, err)

			assert.Equal(t, test.retriable, provider.IsRetriable(err))
			assert.Equal(t, test.limited, provider.IsRateLimited(err))

			failure := provider.FailureOf(err)
			assert.NotNil(t, failure)
			assert.Equal(t, test.status, failure.Status)

			if test.limited {
				delay, ok := provider.RetryAfter(err)
				assert.True(t, ok)
				assert.Equal(t, 7*time.Second, delay)
			}
		})
	}
}

func TestAContextOverflowIsRecognisedFromTheWire(t *testing.T) {
	client := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"This model's maximum context length is 8192 tokens. However, your messages resulted in 9000 tokens.","code":"context_length_exceeded"}}`)
	})

	limit, ok := provider.DetectContextLimit(collect(client, hello()).err)
	assert.True(t, ok)
	assert.Equal(t, 8192, limit.MaxTokens)
}

// The shape of everything sent. The credential and model, the endpoint, the
// limit under the field every OpenAI-compatible server reads, and the tools.
func TestStreamSendsTheRequestAsConfigured(t *testing.T) {
	var seen wireRequest

	client := serve(t, seen.capture)

	call := &fantasy.Call{
		Prompt:          fantasy.Prompt{fantasy.NewSystemMessage("be brief"), fantasy.NewUserMessage("hi")},
		MaxOutputTokens: new(int64(321)),
		Tools: []fantasy.Tool{fantasy.FunctionTool{
			Name: litShell, Description: "run", InputSchema: map[string]any{"type": "object"},
		}},
	}

	require.NoError(t, collect(client, call).err)

	assert.Equal(t, "/chat/completions", seen.path)

	assert.Equal(t, "Bearer test-key", seen.headers.Get("Authorization"))

	stream, _ := seen.body["stream"].(bool)
	assert.Equal(t, litTestModel, seen.body["model"])
	assert.True(t, stream)

	assert.EqualValues(t, 321, seen.body["max_tokens"], "want the limit under the field servers read")

	_, present := seen.body["max_completion_tokens"]
	assert.False(t, present, "max_completion_tokens must not be sent alongside")

	tools, _ := seen.body["tools"].([]any)
	require.Len(t, tools, 1, "want the one offered")
}

// fantasy sends max_completion_tokens for a model whose name looks like a hosted reasoning model, but a local server
// serving one under that name reads max_tokens. That is the field that must go out, whatever the model is called.
func TestTheLimitIsMaxTokensEvenForAReasoningModelName(t *testing.T) {
	var seen wireRequest

	client := serve(t, seen.capture, func(c *provider.ClientConfig) { c.Model = "gpt-5.4" })

	collect(client, &fantasy.Call{Prompt: hello().Prompt, MaxOutputTokens: new(int64(64))})

	assert.EqualValues(t, 64, seen.body["max_tokens"], "want the limit")

	_, present := seen.body["max_completion_tokens"]
	assert.False(t, present, "max_completion_tokens leaked through for a reasoning-looking model name")
}

func TestStreamOmitsTheLimitWhenUnset(t *testing.T) {
	var seen wireRequest

	collect(serve(t, seen.capture), hello())

	for _, field := range []string{"max_tokens", "max_completion_tokens"} {
		_, present := seen.body[field]
		assert.False(t, present, "%s was sent with no limit set", field)
	}
}

// A model name that a hosted provider also uses must not change the wire
// format. This is a chat-completions endpoint whatever the name says.
func TestAHostedModelNameDoesNotSelectAnotherWireFormat(t *testing.T) {
	var seen wireRequest

	collect(serve(t, seen.capture, func(c *provider.ClientConfig) { c.Model = "gpt-5.4" }), hello())

	assert.Equal(t, "/chat/completions", seen.path, "want chat-completions for any model name")
}

// A tool that produced no output still answers its call, and the message
// carries its content. A server that validates the shape rejects one without.
func TestAnEmptyToolResultStillCarriesContent(t *testing.T) {
	prompt := fantasy.Prompt{
		fantasy.NewUserMessage("go"),
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ToolCallPart{ToolCallID: "c1", ToolName: litShell, Input: `{}`},
		}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{ToolCallID: "c1", Output: fantasy.ToolResultOutputContentText{Text: ""}},
		}},
	}

	for _, contentArray := range []bool{false, true} {
		var seen wireRequest

		client := serve(t, seen.capture, func(c *provider.ClientConfig) { c.ContentArray = contentArray })

		collect(client, &fantasy.Call{Prompt: prompt})

		messages := seen.messages()

		tool := messages[len(messages)-1]

		content, present := tool["content"]
		require.True(t, present, "contentArray=%v: the tool message has no content key: %v", contentArray, tool)

		if contentArray {
			parts, ok := content.([]any)
			assert.True(t, ok, "want an empty array")
			assert.Empty(t, parts, "want an empty array")
		} else {
			assert.Empty(t, content, "want an empty string")
		}
	}
}

// Off by default. Every endpoint zot reaches out of the box takes the string.
func TestContentIsAStringUnlessAnArrayIsAskedFor(t *testing.T) {
	var seen wireRequest

	collect(serve(t, seen.capture), &fantasy.Call{Prompt: fantasy.Prompt{
		fantasy.NewSystemMessage("be brief"), fantasy.NewUserMessage("hi"),
	}})

	for _, message := range seen.messages() {
		_, isString := message["content"].(string)
		assert.True(t, isString, "want a plain string by default")
	}
}

func TestContentArrayWrapsEveryMessageInParts(t *testing.T) {
	var seen wireRequest

	client := serve(t, seen.capture, func(c *provider.ClientConfig) { c.ContentArray = true })

	collect(client, &fantasy.Call{Prompt: fantasy.Prompt{
		fantasy.NewSystemMessage("be brief"), fantasy.NewUserMessage("hi"),
	}})

	messages := seen.messages()
	require.Len(t, messages, 2)

	for index, want := range []string{"be brief", "hi"} {
		parts, ok := messages[index]["content"].([]any)
		require.True(t, ok, "want one part")
		require.Len(t, parts, 1, "want one part")

		part, ok := parts[0].(map[string]any)
		require.True(t, ok, "message %d part is %T, want an object", index, parts[0])

		assert.Equal(t, "text", part["type"], "message %d part", index)
		assert.Equal(t, want, part["text"], "message %d part", index)
	}
}

func TestNoAmbientCredentialReachesTheWire(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-from-the-environment")
	t.Setenv("OPENAI_ORG_ID", "org-ambient")
	t.Setenv("OPENAI_PROJECT_ID", "proj-ambient")

	var seen wireRequest

	client := serve(t, seen.capture, func(c *provider.ClientConfig) { c.APIKey = "" })

	// loopback, so no key is required, and none may be invented
	require.NoError(t, collect(client, hello()).err)

	for _, header := range []string{"Authorization", "OpenAI-Organization", "OpenAI-Project"} {
		assert.Empty(t, seen.headers.Get(header), "want nothing the operator did not configure")
	}
}

func TestAConfiguredKeyIsNotReplacedByTheEnvironment(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-from-the-environment")

	var seen wireRequest

	collect(serve(t, seen.capture), hello())

	assert.Equal(t, "Bearer test-key", seen.headers.Get("Authorization"), "want the configured key")
}

func TestClientExposesItsResolvedConfig(t *testing.T) {
	client := serve(t, func(http.ResponseWriter, *http.Request) {}, func(c *provider.ClientConfig) { c.BaseURL += "/" })

	config := client.Config()

	assert.Equal(t, litTestModel, config.Model)
	assert.False(t, strings.HasSuffix(config.BaseURL, "/"))
}

func TestNewRefusesAnInvalidConfig(t *testing.T) {
	_, err := provider.NewClient(t.Context(), provider.ClientConfig{})
	require.Error(t, err, "an empty config must not connect")
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

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan struct{})

	go func() {
		defer close(done)

		for part := range streamOf(ctx, client, hello()) {
			_ = part
		}
	}()

	time.Sleep(50 * time.Millisecond)

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "canceling the context did not end the stream")
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

	ctx, cancel := context.WithCancel(t.Context())

	for part := range streamOf(ctx, client, hello()) {
		if part.Type == fantasy.StreamPartTypeTextDelta {
			break
		}
	}

	cancel()

	select {
	case <-released:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the server never saw the connection go away")
	}
}

// A long turn is not a hung one. Only silence is bounded, so a stream that keeps
// producing must not be cut off however long it runs.
func TestASlowButProgressingStreamIsNotCutOff(t *testing.T) {
	withStallTimeout(t, 300*time.Millisecond)

	client := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		for range 20 {
			fmt.Fprint(w, "data: "+`{"choices":[{"delta":{"content":"x"}}]}`+"\n\n")

			w.(http.Flusher).Flush()

			time.Sleep(50 * time.Millisecond)
		}

		fmt.Fprint(w, sse(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`))
	})

	result := collect(client, hello())

	require.NoError(t, result.err, "a stream that kept producing was cut off")

	assert.Equal(t, strings.Repeat("x", 20), result.text)
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
		require.Error(t, result.err, "a stream that went silent must not hang forever")

		assert.True(t, provider.IsRetriable(result.err), "a stalled stream should be retriable")
	case <-time.After(5 * time.Second):
		require.FailNow(t, "a stream that went silent was never cut off")
	}
}

// The silence bound covers the error path too. A server that sends a status and
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
		require.Error(t, err, "a refused request must report an error")
	case <-time.After(5 * time.Second):
		require.FailNow(t, "an error response held open wedged the turn")
	}
}
