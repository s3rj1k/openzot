package loop_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/conversation"
	"github.com/openzot/openzot/internal/loop"
	"github.com/openzot/openzot/internal/provider"
)

// testWindow is the context window every test engine is given. A window is
// required, and this one is large enough that no test trims by accident.
const testWindow = 1_000_000

// stub serves scripted turns over the OpenAI-compatible wire format, so the loop
// can be driven without a model.
func stub(t *testing.T, turns ...[]string) *provider.Client {
	t.Helper()

	turn := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		index := turn
		if index >= len(turns) {
			index = len(turns) - 1
		}

		turn++

		for _, frame := range turns[index] {
			fmt.Fprintf(w, "data: %s\n\n", frame)
		}

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

	return client
}

func text(s string) string {
	return fmt.Sprintf(`{"choices":[{"delta":{"content":%q}}]}`, s)
}

func stop() string {
	return `{"choices":[{"delta":{},"finish_reason":"stop"}]}`
}

func truncated() string {
	return `{"choices":[{"delta":{},"finish_reason":"length"}]}`
}

// usageFrame is a final chunk carrying the provider's own token counts.
func usageFrame(prompt, completion int) string {
	return fmt.Sprintf(
		`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
		prompt, completion, prompt+completion,
	)
}

func tool(id, name, arguments string) string {
	return fmt.Sprintf(
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":"tool_calls"}]}`,
		id, name, arguments,
	)
}

// settle is a turn that ends the run. The model calls the success tool.
func settle(summary string) string {
	return tool("done", loop.SuccessTool, fmt.Sprintf(`{"summary":%q}`, summary))
}

func run(t *testing.T, options *loop.Options) loop.Result {
	t.Helper()

	// Tests skip the retry backoff unless they are about it. A test driving a retriable failure should not
	// sleep through the production default. A test of the backoff sets its own value.
	if options.RetryBackoff == 0 {
		options.RetryBackoff = -1
	}

	engine, err := loop.New(options)
	require.NoError(t, err)

	return engine.Run(t.Context(), nil)
}

// noInput is the argument struct of the engine tests' tools, which take none.
type noInput struct{}

// namedTool is a tool for engine tests. It takes no arguments and answers with
// what the handler returns. A handler error is reported the way the real tools
// report a failure - as an error response, not a critical error.
func namedTool(name string, handler func(context.Context) (any, error)) fantasy.AgentTool {
	return fantasy.NewAgentTool(name, name,
		func(ctx context.Context, _ noInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			output, err := handler(ctx)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			if output == nil {
				return fantasy.NewTextResponse(""), nil
			}

			return fantasy.NewTextResponse(fmt.Sprint(output)), nil
		})
}

func echoTool(calls *int) []fantasy.AgentTool {
	return []fantasy.AgentTool{namedTool(litEcho, func(context.Context) (any, error) {
		*calls++

		return "ok", nil
	})}
}

func TestNewAppliesDefaults(t *testing.T) {
	engine, err := loop.New(&loop.Options{ContextWindow: testWindow, Client: stub(t, []string{stop()})})
	require.NoError(t, err)

	assert.Equal(t, loop.DefaultMaxIterations, engine.MaxIterations)
	assert.Equal(t, loop.DefaultMaxContinuations, engine.MaxContinuations)
	assert.Equal(t, loop.DefaultMaxCycles, engine.MaxCycles)
	assert.Equal(t, loop.DefaultMaxEmpties, engine.MaxEmpties)

	// calls and time are unbounded unless set - only the iteration count is a
	// hard default fallback
	assert.Equal(t, 0, engine.MaxCalls, "want 0 (unbounded) by default")

	assert.EqualValues(t, 0, engine.MaxDuration, "want unbounded by default")

	// Settlement cannot be switched off. An unattended run needs an unambiguous
	// ending, so an unset budget is the default budget, never "no settling".
	assert.Equal(t, loop.DefaultMaxSettles, engine.MaxSettles, "maxSettles = %d, want the default %d - there is no way to opt out", engine.MaxSettles, loop.DefaultMaxSettles)

	assert.Equal(t, testWindow, engine.Window, "want the configured window and the default thresholds")
	assert.Equal(t, loop.DefaultContextSoft, engine.SoftPercent, "want the configured window and the default thresholds")
	assert.Equal(t, loop.DefaultContextHard, engine.HardPercent, "want the configured window and the default thresholds")
}

func TestNewRequiresAClient(t *testing.T) {
	_, err := loop.New(&loop.Options{ContextWindow: testWindow})
	require.Error(t, err, "an engine without a client must not be constructed")
}

func TestIterationBudgetStopsTheRun(t *testing.T) {
	calls := 0

	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client:        stub(t, []string{tool("c1", litEcho, `{}`)}),
		Tools:         echoTool(&calls),
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxIterations: 3,

		// disable the cycle guard so the iteration cap is what fires
		MaxCycles: 1000,
	})

	assert.Equal(t, loop.StopIterations, result.Reason)

	assert.Equal(t, 3, result.Budget.Iterations)
}

func TestCallBudgetStopsTheRun(t *testing.T) {
	calls := 0

	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client:        stub(t, []string{tool("c1", litEcho, `{}`)}),
		Tools:         echoTool(&calls),
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxCalls:      2,
		MaxIterations: 50,
		MaxCycles:     1000,
	})

	assert.Equal(t, loop.StopCalls, result.Reason)

	assert.LessOrEqual(t, calls, 2, "want at most the budget of 2")
}

func TestEmptyTurnsAreBounded(t *testing.T) {
	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client:        stub(t, []string{stop()}),
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxEmpties:    2,
	})

	assert.Equal(t, loop.StopEmpty, result.Reason)

	assert.Equal(t, 2, result.Budget.Empties)
}

func TestTruncatedOutputIsContinued(t *testing.T) {
	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client: stub(t,
			[]string{text("half an answ"), truncated()},
			[]string{settle("er, continued")},
		),
		Messages: []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
	})

	assert.Equal(t, loop.StopSettled, result.Reason)

	assert.Equal(t, 1, result.Budget.Recoveries)

	// the continuation notice must be in the thread, telling the model to pick
	// up where it stopped rather than start again
	var nudged bool

	for _, message := range result.Messages {
		if strings.Contains(message.Text, "cut off at the output limit") {
			nudged = true
		}
	}

	assert.True(t, nudged, "a truncated turn must be followed by a continuation notice")
}

func TestTruncationIsBounded(t *testing.T) {
	result := run(t, &loop.Options{
		ContextWindow:    testWindow,
		Client:           stub(t, []string{text("x"), truncated()}),
		Messages:         []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxContinuations: 2,
	})

	assert.Equal(t, loop.StopContinuations, result.Reason)
}

func TestRepeatedToolResultsTripTheCycleGuard(t *testing.T) {
	calls := 0

	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client:        stub(t, []string{tool("c1", litEcho, `{"q":"same"}`)}),
		Tools:         echoTool(&calls),
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxIterations: 50,
		MaxCycles:     1,
	})

	assert.Equal(t, loop.StopCycle, result.Reason)

	// the run must have been nudged before being stopped
	assert.Equal(t, 1, result.Budget.Cycles)
}

func TestSettleModeRequiresATerminalCall(t *testing.T) {
	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client: stub(t,
			[]string{text("All done, the task is completed."), stop()},
			[]string{tool("c9", loop.SuccessTool, `{"summary":"really done"}`)},
		),
		Messages:   []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxSettles: 5,
	})

	require.Equal(t, loop.StopSettled, result.Reason)

	assert.Equal(t, "really done", result.Message, "want the terminal call's summary")

	assert.Equal(t, 1, result.Budget.Settles, "want 1 nudge before the terminal call")
}

func TestSettleModeFailureToolAlsoEnds(t *testing.T) {
	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client:        stub(t, []string{tool("c9", loop.FailureTool, `{"reason":"cannot reach the host"}`)}),
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxSettles:    5,
	})

	assert.Equal(t, loop.StopFailed, result.Reason)

	assert.Equal(t, "cannot reach the host", result.Message)
}

// The two terminal tools mean opposite things, so a caller must tell them apart. Both once ended a run as StopSettled, which
// exits 0, so a mission the model gave up on was reported to scripts, schedules and the log as a success.
func TestTerminalToolsReportOppositeOutcomes(t *testing.T) {
	settled := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client:        stub(t, []string{tool("c1", loop.SuccessTool, `{"summary":"shipped it"}`)}),
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxSettles:    5,
	})

	failed := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client:        stub(t, []string{tool("c9", loop.FailureTool, `{"reason":"cannot reach the host"}`)}),
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxSettles:    5,
	})

	require.NotEqual(t, failed.Reason, settled.Reason, "both terminal tools ended the run as %q - nothing downstream can tell a failed mission from a finished one", settled.Reason)

	assert.Equal(t, loop.StopSettled, settled.Reason)

	assert.Equal(t, loop.StopFailed, failed.Reason)
}

func TestSettleModeGivesUpEventually(t *testing.T) {
	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client:        stub(t, []string{text("I believe I am finished."), stop()}),
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxSettles:    2,
	})

	assert.Equal(t, loop.StopUnsettled, result.Reason)
}

func TestCancellationStopsTheRun(t *testing.T) {
	engine, err := loop.New(&loop.Options{
		ContextWindow: testWindow,
		Client:        stub(t, []string{text("hi"), stop()}),
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())

	cancel()

	result := engine.Run(ctx, nil)

	assert.Equal(t, loop.StopAborted, result.Reason)
}

func TestUnknownToolIsFedBackNotFatal(t *testing.T) {
	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client: stub(t,
			[]string{tool("c1", "missing", `{}`)},
			[]string{settle("recovered")},
		),
		Messages: []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
	})

	assert.Equal(t, loop.StopSettled, result.Reason, "want the run to recover and stop normally")

	var reported bool

	for _, message := range result.Messages {
		if strings.Contains(message.Text, "tool not found") {
			reported = true
		}
	}

	assert.True(t, reported, "the failure must be fed back to the model")
}

func TestToolErrorIsFedBackNotFatal(t *testing.T) {
	tools := []fantasy.AgentTool{namedTool("boom", func(context.Context) (any, error) {
		return nil, errors.New("disk on fire")
	})}

	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client: stub(t,
			[]string{tool("c1", "boom", `{}`)},
			[]string{settle("noted")},
		),
		Tools:    tools,
		Messages: []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
	})

	var reported bool

	for _, message := range result.Messages {
		if strings.Contains(message.Text, "disk on fire") {
			reported = true
		}
	}

	assert.True(t, reported, "a tool failure must reach the model so it can adapt")
}

func TestEventsAreEmitted(t *testing.T) {
	engine, err := loop.New(&loop.Options{
		ContextWindow: testWindow,
		Client:        stub(t, []string{text("hello"), tool("c1", loop.SuccessTool, `{"summary":"done"}`)}),
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
	})
	require.NoError(t, err)

	var kinds []loop.EventKind

	engine.Run(t.Context(), func(event loop.Event) {
		kinds = append(kinds, event.Kind)
	})

	want := map[loop.EventKind]bool{loop.EventIteration: false, loop.EventToken: false, loop.EventMessage: false}

	for _, kind := range kinds {
		if _, tracked := want[kind]; tracked {
			want[kind] = true
		}
	}

	for kind, seen := range want {
		assert.True(t, seen, "no %s event was emitted", kind)
	}
}

// The terminal tools are always offered. The model cannot settle without them.
func TestTheTerminalToolsAreAlwaysOffered(t *testing.T) {
	engine, err := loop.New(&loop.Options{
		ContextWindow: testWindow,
		Client:        stub(t, []string{stop()}),
		Tools:         []fantasy.AgentTool{namedTool(litEcho, nil)},
	})
	require.NoError(t, err)

	names := map[string]bool{}

	for _, tool := range engine.ToolDefinitions() {
		names[tool.GetName()] = true
	}

	for _, want := range []string{litEcho, loop.SuccessTool, loop.FailureTool} {
		assert.True(t, names[want], "tool %q missing from the definitions", want)
	}
}

// The tool list must not depend on the order tools were handed in. A list that
// reshuffles between requests defeats a server-side prompt cache keyed on the
// prefix, so the order must be fixed.
func TestToolDefinitionsAreOrderedByName(t *testing.T) {
	tools := make([]fantasy.AgentTool, 0, 5)

	for _, name := range []string{"write", litRead, "shell", "list", "edit"} {
		tools = append(tools, namedTool(name, nil))
	}

	engine, err := loop.New(&loop.Options{ContextWindow: testWindow, Client: stub(t, []string{stop()}), Tools: tools})
	require.NoError(t, err)

	for range 20 {
		names := make([]string, 0, len(engine.ToolDefinitions()))

		for _, tool := range engine.ToolDefinitions() {
			names = append(names, tool.GetName())
		}

		require.Equal(t, "edit,failure,list,read,shell,success,write", strings.Join(names, ","))
	}
}

// The runaway guard ends a turn while the provider is still streaming, so the stream it walks away from must be canceled.
// It was not, and the transport goroutine held the response body open for the life of the process, leaking once per guard trip.
func TestAnAbandonedStreamIsCancelled(t *testing.T) {
	canceled := make(chan struct{})

	done := sync.OnceFunc(func() { close(canceled) })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		flusher, _ := w.(http.Flusher)

		// stream a repeating phrase forever. Past RunawayGuardMinChars the guard
		// recognizes the repetition and cuts the turn short mid-stream
		for {
			select {
			case <-r.Context().Done():
				done()

				return
			default:
			}

			if _, err := fmt.Fprintf(w, "data: %s\n\n", text("the same sentence over and over again. ")); err != nil {
				done()

				return
			}

			if flusher != nil {
				flusher.Flush()
			}
		}
	}))

	t.Cleanup(server.Close)

	client, err := provider.NewClient(t.Context(), provider.ClientConfig{
		Provider: litCustom,
		Model:    litTestModel,
		APIKey:   "k",
		BaseURL:  server.URL,
	})
	require.NoError(t, err)

	engine, err := loop.New(&loop.Options{
		ContextWindow: testWindow,
		Client:        client,
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxIterations: 1,
	})
	require.NoError(t, err)

	var runaway bool

	engine.Run(t.Context(), func(event loop.Event) {
		if event.Kind == loop.EventRunaway {
			runaway = true
		}
	})

	require.True(t, runaway, "the guard never tripped, so this test is not exercising an abandoned stream")

	select {
	case <-canceled:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "the abandoned stream was never canceled: its transport goroutine and response body leak")
	}
}

// The window is the operator's to state. There is no table of what models can
// take, and a serving endpoint's real ceiling can be smaller than any model's
// card. Forgetting follows the window that was given.
func TestTheWindowIsTheConfiguredOne(t *testing.T) {
	engine, err := loop.New(&loop.Options{Client: stub(t, []string{stop()}), ContextWindow: 32_000})
	require.NoError(t, err)

	assert.Equal(t, 32_000, engine.Window)
}

// The thresholds are the operator's, and zero means the default. Whether they
// make sense together is the config's to say, so the engine takes what it is given.
func TestContextThresholdsDefaultWhenUnset(t *testing.T) {
	engine, err := loop.New(&loop.Options{Client: stub(t, []string{stop()}), ContextWindow: 1000})
	require.NoError(t, err)

	assert.Equal(t, loop.DefaultContextSoft, engine.SoftPercent)
	assert.Equal(t, loop.DefaultContextHard, engine.HardPercent)

	engine, err = loop.New(&loop.Options{Client: stub(t, []string{stop()}), ContextWindow: 1000, ContextSoft: 30, ContextHard: 60})
	require.NoError(t, err)

	assert.Equal(t, 30, engine.SoftPercent)
	assert.Equal(t, 60, engine.HardPercent)
}

// A run with no window has nothing to decide how much of a conversation to keep,
// and guessing one is exactly what the operator is asked not to leave to agent.
func TestNewRefusesARunWithoutAWindow(t *testing.T) {
	client := stub(t, []string{stop()})

	for _, window := range []int{0, -1} {
		if _, err := loop.New(&loop.Options{Client: client, ContextWindow: window}); err == nil {
			assert.Failf(t, "unexpected", "a context window of %d was accepted", window)
		} else if !strings.Contains(err.Error(), "context") {
			assert.Failf(t, "unexpected", "the error should say a context window is missing: %v", err)
		}
	}
}

// Forgetting takes the oldest first, which is the run's opening user message, and a conversation with no user turn is rejected
// wholesale by strict providers from then on. The request must always carry a user turn.
func TestATrimmedThreadStillCarriesAUserTurn(t *testing.T) {
	engine, err := loop.New(&loop.Options{
		Client: stub(t, []string{stop()}),
		// a window small enough that the oldest messages must be forgotten
		ContextWindow: 20_000,
	})
	require.NoError(t, err)

	// an old user kickoff followed by enough tool-round bulk to evict it
	messages := make([]conversation.Message, 0, 1+2*40)
	messages = append(messages, conversation.Message{Type: conversation.TypeUser, Text: "the kickoff"})

	for i := range 40 {
		id := fmt.Sprintf("c%d", i)
		messages = append(messages,
			conversation.Message{Type: conversation.TypeActivity, Activity: &conversation.Activity{Kind: conversation.ActivityRequest, ID: id, Name: litRead, Arguments: `{"path":"x"}`}},
			conversation.Message{Type: conversation.TypeActivity, Activity: &conversation.Activity{Kind: conversation.ActivityResponse, ID: id, Name: litRead, Result: strings.Repeat("line of file content ", 200)}},
		)
	}

	request, _ := requestFor(engine, messages)

	for _, message := range request.Messages {
		require.NotEqual(t, fantasy.MessageRoleSystem, message.Role, "the system prompt travels with the agent, not in the conversation")
	}

	var hasUser bool

	var kept int

	for _, message := range request.Messages {
		if message.Role == fantasy.MessageRoleUser {
			hasUser = true
		}

		kept++
	}

	require.Less(t, kept, 1+2*40, "nothing was trimmed; the test needs a smaller window to mean anything")

	assert.True(t, hasUser, "a trimmed thread lost its only user turn; strict providers reject the whole request")
}

// A run killed inside a tool call still leaves the turn that made it. The
// reasoning, the words and the request are handed over before the handler runs,
// not at the next iteration boundary the killed run never reaches.
func TestTheTurnIsHandedOverBeforeItsToolRuns(t *testing.T) {
	var handed [][]conversation.Message

	var seenByHandler []conversation.Message

	tools := []fantasy.AgentTool{namedTool(litEcho, func(context.Context) (any, error) {
		seenByHandler = handed[len(handed)-1]

		return "ok", nil
	})}

	run(t, &loop.Options{
		Client: stub(t,
			[]string{
				`{"choices":[{"delta":{"reasoning_content":"the file is probably in src"}}]}`,
				text("looking"),
				tool("c1", litEcho, "{}"),
			},
			[]string{settle("done")},
		),
		Tools:         tools,
		ContextWindow: testWindow,
		OnConversation: func(messages []conversation.Message) {
			handed = append(handed, append([]conversation.Message(nil), messages...))
		},
	})

	got := make([]string, 0, len(seenByHandler))

	for _, message := range seenByHandler {
		got = append(got, string(message.Type)+"/"+message.Text)
	}

	want := []string{"reasoning/the file is probably in src", "bot/looking"}

	require.GreaterOrEqual(t, len(got), len(want)+1, "the handler ran when only %v had been handed over", got)
	require.Equal(t, want[0], got[len(got)-3], "the handler ran when only %v had been handed over", got)
	require.Equal(t, want[1], got[len(got)-2], "the handler ran when only %v had been handed over", got)

	last := seenByHandler[len(seenByHandler)-1]
	assert.NotNil(t, last.Activity, "the request must be handed over with its turn, got %+v", last)
	assert.Equal(t, conversation.ActivityRequest, last.Activity.Kind, "the request must be handed over with its turn, got %+v", last)
}
