package loop

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

	"github.com/openzot/openzot/internal/conversation"
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
	if err != nil {
		t.Fatalf("New: %v", err)
	}

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
	return tool("done", SuccessTool, fmt.Sprintf(`{"summary":%q}`, summary))
}

func run(t *testing.T, options *Options) Result {
	t.Helper()

	// Tests skip the retry backoff unless they are about it. A test driving a retriable failure should not
	// sleep through the production default. A test of the backoff sets its own value.
	if options.RetryBackoff == 0 {
		options.RetryBackoff = -1
	}

	engine, err := New(options)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

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
	engine, err := New(&Options{ContextWindow: testWindow, Client: stub(t, []string{stop()})})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if engine.maxIterations != DefaultMaxIterations ||
		engine.maxContinuations != DefaultMaxContinuations ||
		engine.maxCycles != DefaultMaxCycles ||
		engine.maxEmpties != DefaultMaxEmpties {
		t.Errorf("defaults not applied: %+v", engine)
	}

	// calls and time are unbounded unless set - only the iteration count is a
	// hard default fallback
	if engine.maxCalls != 0 {
		t.Errorf("maxCalls = %d, want 0 (unbounded) by default", engine.maxCalls)
	}

	if engine.maxDuration != 0 {
		t.Errorf("maxDuration = %v, want unbounded by default", engine.maxDuration)
	}

	// Settlement cannot be switched off. An unattended run needs an unambiguous
	// ending, so an unset budget is the default budget, never "no settling".
	if engine.maxSettles != DefaultMaxSettles {
		t.Errorf("maxSettles = %d, want the default %d - there is no way to opt out", engine.maxSettles, DefaultMaxSettles)
	}

	if engine.window != testWindow || engine.softPercent != DefaultContextSoft || engine.hardPercent != DefaultContextHard {
		t.Errorf("window %d, thresholds %d/%d, want the configured window and the default thresholds",
			engine.window, engine.softPercent, engine.hardPercent)
	}
}

func TestNewRequiresAClient(t *testing.T) {
	if _, err := New(&Options{ContextWindow: testWindow}); err == nil {
		t.Fatal("an engine without a client must not be constructed")
	}
}

func TestIterationBudgetStopsTheRun(t *testing.T) {
	calls := 0

	result := run(t, &Options{
		ContextWindow: testWindow,
		Client:        stub(t, []string{tool("c1", litEcho, `{}`)}),
		Tools:         echoTool(&calls),
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxIterations: 3,

		// disable the cycle guard so the iteration cap is what fires
		MaxCycles: 1000,
	})

	if result.Reason != StopIterations {
		t.Errorf("reason = %q, want iterations", result.Reason)
	}

	if result.Budget.Iterations != 3 {
		t.Errorf("iterations = %d, want 3", result.Budget.Iterations)
	}
}

func TestCallBudgetStopsTheRun(t *testing.T) {
	calls := 0

	result := run(t, &Options{
		ContextWindow: testWindow,
		Client:        stub(t, []string{tool("c1", litEcho, `{}`)}),
		Tools:         echoTool(&calls),
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxCalls:      2,
		MaxIterations: 50,
		MaxCycles:     1000,
	})

	if result.Reason != StopCalls {
		t.Errorf("reason = %q, want calls", result.Reason)
	}

	if calls > 2 {
		t.Errorf("handler ran %d times, want at most the budget of 2", calls)
	}
}

func TestEmptyTurnsAreBounded(t *testing.T) {
	result := run(t, &Options{
		ContextWindow: testWindow,
		Client:        stub(t, []string{stop()}),
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxEmpties:    2,
	})

	if result.Reason != StopEmpty {
		t.Errorf("reason = %q, want empty", result.Reason)
	}

	if result.Budget.Empties != 2 {
		t.Errorf("empties = %d, want 2", result.Budget.Empties)
	}
}

func TestTruncatedOutputIsContinued(t *testing.T) {
	result := run(t, &Options{
		ContextWindow: testWindow,
		Client: stub(t,
			[]string{text("half an answ"), truncated()},
			[]string{settle("er, continued")},
		),
		Messages: []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
	})

	if result.Reason != StopSettled {
		t.Errorf("reason = %q, want stop", result.Reason)
	}

	if result.Budget.Recoveries != 1 {
		t.Errorf("continuations = %d, want 1", result.Budget.Recoveries)
	}

	// the continuation notice must be in the thread, telling the model to pick
	// up where it stopped rather than start again
	var nudged bool

	for _, message := range result.Messages {
		if strings.Contains(message.Text, "cut off at the output limit") {
			nudged = true
		}
	}

	if !nudged {
		t.Error("a truncated turn must be followed by a continuation notice")
	}
}

func TestTruncationIsBounded(t *testing.T) {
	result := run(t, &Options{
		ContextWindow:    testWindow,
		Client:           stub(t, []string{text("x"), truncated()}),
		Messages:         []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxContinuations: 2,
	})

	if result.Reason != StopContinuations {
		t.Errorf("reason = %q, want continuations", result.Reason)
	}
}

func TestRepeatedToolResultsTripTheCycleGuard(t *testing.T) {
	calls := 0

	result := run(t, &Options{
		ContextWindow: testWindow,
		Client:        stub(t, []string{tool("c1", litEcho, `{"q":"same"}`)}),
		Tools:         echoTool(&calls),
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxIterations: 50,
		MaxCycles:     1,
	})

	if result.Reason != StopCycle {
		t.Errorf("reason = %q, want cycle", result.Reason)
	}

	// the run must have been nudged before being stopped
	if result.Budget.Cycles != 1 {
		t.Errorf("cycles = %d, want 1", result.Budget.Cycles)
	}
}

func TestSettleModeRequiresATerminalCall(t *testing.T) {
	result := run(t, &Options{
		ContextWindow: testWindow,
		Client: stub(t,
			[]string{text("All done, the task is completed."), stop()},
			[]string{tool("c9", SuccessTool, `{"summary":"really done"}`)},
		),
		Messages:   []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxSettles: 5,
	})

	if result.Reason != StopSettled {
		t.Fatalf("reason = %q, want settled", result.Reason)
	}

	if result.Message != "really done" {
		t.Errorf("message = %q, want the terminal call's summary", result.Message)
	}

	if result.Budget.Settles != 1 {
		t.Errorf("settles = %d, want 1 nudge before the terminal call", result.Budget.Settles)
	}
}

func TestSettleModeFailureToolAlsoEnds(t *testing.T) {
	result := run(t, &Options{
		ContextWindow: testWindow,
		Client:        stub(t, []string{tool("c9", FailureTool, `{"reason":"cannot reach the host"}`)}),
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxSettles:    5,
	})

	if result.Reason != StopFailed {
		t.Errorf("reason = %q, want failed", result.Reason)
	}

	if result.Message != "cannot reach the host" {
		t.Errorf("message = %q, want the failure reason", result.Message)
	}
}

// The two terminal tools mean opposite things, so a caller has to be able to
// tell them apart. Both used to end a run as StopSettled, which exits 0 and
// renders as "done" - a mission the model gave up on was reported to scripts,
// schedules and the session log as a success.
func TestTerminalToolsReportOppositeOutcomes(t *testing.T) {
	settled := run(t, &Options{
		ContextWindow: testWindow,
		Client:        stub(t, []string{tool("c1", SuccessTool, `{"summary":"shipped it"}`)}),
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxSettles:    5,
	})

	failed := run(t, &Options{
		ContextWindow: testWindow,
		Client:        stub(t, []string{tool("c9", FailureTool, `{"reason":"cannot reach the host"}`)}),
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxSettles:    5,
	})

	if settled.Reason == failed.Reason {
		t.Fatalf("both terminal tools ended the run as %q - nothing downstream can tell a failed mission from a finished one", settled.Reason)
	}

	if settled.Reason != StopSettled {
		t.Errorf("success reason = %q, want settled", settled.Reason)
	}

	if failed.Reason != StopFailed {
		t.Errorf("failure reason = %q, want failed", failed.Reason)
	}
}

func TestSettleModeGivesUpEventually(t *testing.T) {
	result := run(t, &Options{
		ContextWindow: testWindow,
		Client:        stub(t, []string{text("I believe I am finished."), stop()}),
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxSettles:    2,
	})

	if result.Reason != StopUnsettled {
		t.Errorf("reason = %q, want unsettled", result.Reason)
	}
}

func TestCancellationStopsTheRun(t *testing.T) {
	engine, err := New(&Options{
		ContextWindow: testWindow,
		Client:        stub(t, []string{text("hi"), stop()}),
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())

	cancel()

	result := engine.Run(ctx, nil)

	if result.Reason != StopAborted {
		t.Errorf("reason = %q, want aborted", result.Reason)
	}
}

func TestUnknownToolIsFedBackNotFatal(t *testing.T) {
	result := run(t, &Options{
		ContextWindow: testWindow,
		Client: stub(t,
			[]string{tool("c1", "missing", `{}`)},
			[]string{settle("recovered")},
		),
		Messages: []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
	})

	if result.Reason != StopSettled {
		t.Errorf("reason = %q, want the run to recover and stop normally", result.Reason)
	}

	var reported bool

	for _, message := range result.Messages {
		if strings.Contains(message.Text, "tool not found") {
			reported = true
		}
	}

	if !reported {
		t.Error("the failure must be fed back to the model")
	}
}

func TestToolErrorIsFedBackNotFatal(t *testing.T) {
	tools := []fantasy.AgentTool{namedTool("boom", func(context.Context) (any, error) {
		return nil, errors.New("disk on fire")
	})}

	result := run(t, &Options{
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

	if !reported {
		t.Error("a tool failure must reach the model so it can adapt")
	}
}

func TestEventsAreEmitted(t *testing.T) {
	engine, err := New(&Options{
		ContextWindow: testWindow,
		Client:        stub(t, []string{text("hello"), tool("c1", SuccessTool, `{"summary":"done"}`)}),
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var kinds []EventKind

	engine.Run(t.Context(), func(event Event) {
		kinds = append(kinds, event.Kind)
	})

	want := map[EventKind]bool{EventIteration: false, EventToken: false, EventMessage: false}

	for _, kind := range kinds {
		if _, tracked := want[kind]; tracked {
			want[kind] = true
		}
	}

	for kind, seen := range want {
		if !seen {
			t.Errorf("no %s event was emitted", kind)
		}
	}
}

func TestInstructionsRendersTheSettleInstruction(t *testing.T) {
	engine, err := New(&Options{
		ContextWindow: testWindow,
		Client:        stub(t, []string{stop()}),
		Instructions:  "you are an agent",
		MaxSettles:    5,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	instructions := engine.instructions()

	for _, want := range []string{
		"you are an agent",
		SuccessTool,
		FailureTool,
	} {
		if !strings.Contains(instructions, want) {
			t.Errorf("instructions is missing %q:\n%s", want, instructions)
		}
	}
}

// The terminal tools are always offered. The model cannot settle without them.
func TestTheTerminalToolsAreAlwaysOffered(t *testing.T) {
	engine, err := New(&Options{
		ContextWindow: testWindow,
		Client:        stub(t, []string{stop()}),
		Tools:         []fantasy.AgentTool{namedTool(litEcho, nil)},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	names := map[string]bool{}

	for _, tool := range engine.toolDefinitions() {
		names[tool.GetName()] = true
	}

	for _, want := range []string{litEcho, SuccessTool, FailureTool} {
		if !names[want] {
			t.Errorf("tool %q missing from the definitions", want)
		}
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

	engine, err := New(&Options{ContextWindow: testWindow, Client: stub(t, []string{stop()}), Tools: tools})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for range 20 {
		names := make([]string, 0, len(engine.toolDefinitions()))

		for _, tool := range engine.toolDefinitions() {
			names = append(names, tool.GetName())
		}

		if want := "edit,failure,list,read,shell,success,write"; strings.Join(names, ",") != want {
			t.Fatalf("tool order = %v, want %s", names, want)
		}
	}
}

// The runaway guard ends a turn while the provider is still streaming, so the
// stream it walks away from has to be canceled. It was not. The transport's
// producer goroutine stayed parked on a send nobody would ever receive, holding
// its HTTP response body open for the life of the process, and every trip of the
// guard - a routine event in a long run, which is why the guard exists - leaked
// another one.
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
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	engine, err := New(&Options{
		ContextWindow: testWindow,
		Client:        client,
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxIterations: 1,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var runaway bool

	engine.Run(t.Context(), func(event Event) {
		if event.Kind == EventRunaway {
			runaway = true
		}
	})

	if !runaway {
		t.Fatal("the guard never tripped, so this test is not exercising an abandoned stream")
	}

	select {
	case <-canceled:
	case <-time.After(10 * time.Second):
		t.Fatal("the abandoned stream was never canceled: its transport goroutine and response body leak")
	}
}

// The window is the operator's to state. There is no table of what models can
// take, and a serving endpoint's real ceiling can be smaller than any model's
// card. Forgetting follows the window that was given.
func TestTheWindowIsTheConfiguredOne(t *testing.T) {
	engine, err := New(&Options{Client: stub(t, []string{stop()}), ContextWindow: 32_000})
	if err != nil {
		t.Fatal(err)
	}

	if engine.window != 32_000 {
		t.Errorf("window = %d, want the configured 32000", engine.window)
	}
}

// The thresholds are the operator's, and zero means the default. Whether they
// make sense together is the config's to say, so the engine takes what it is given.
func TestContextThresholdsDefaultWhenUnset(t *testing.T) {
	engine, err := New(&Options{Client: stub(t, []string{stop()}), ContextWindow: 1000})
	if err != nil {
		t.Fatal(err)
	}

	if engine.softPercent != DefaultContextSoft || engine.hardPercent != DefaultContextHard {
		t.Errorf("thresholds = %d/%d, want the defaults %d/%d",
			engine.softPercent, engine.hardPercent, DefaultContextSoft, DefaultContextHard)
	}

	engine, err = New(&Options{Client: stub(t, []string{stop()}), ContextWindow: 1000, ContextSoft: 30, ContextHard: 60})
	if err != nil {
		t.Fatal(err)
	}

	if engine.softPercent != 30 || engine.hardPercent != 60 {
		t.Errorf("thresholds = %d/%d, want the configured 30/60", engine.softPercent, engine.hardPercent)
	}
}

// A run with no window has nothing to decide how much of a conversation to keep,
// and guessing one is exactly what the operator is asked not to leave to zot.
func TestNewRefusesARunWithoutAWindow(t *testing.T) {
	client := stub(t, []string{stop()})

	for _, window := range []int{0, -1} {
		if _, err := New(&Options{Client: client, ContextWindow: window}); err == nil {
			t.Errorf("a context window of %d was accepted", window)
		} else if !strings.Contains(err.Error(), "context") {
			t.Errorf("the error should say a context window is missing: %v", err)
		}
	}
}

// Forgetting takes the oldest first, which is the run's opening user message -
// leaving a conversation with no
// user turn at all, which strict providers reject wholesale with an opaque
// 400 from that iteration on (bisected live. The same request with one
// user message injected was accepted). The request must always carry a user
// turn.
func TestATrimmedThreadStillCarriesAUserTurn(t *testing.T) {
	engine, err := New(&Options{
		Client: stub(t, []string{stop()}),
		// a window small enough that the oldest messages must be forgotten
		ContextWindow: 20_000,
	})
	if err != nil {
		t.Fatal(err)
	}

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

	for _, message := range request.messages {
		if message.Role == fantasy.MessageRoleSystem {
			t.Fatal("the system prompt travels with the agent, not in the conversation")
		}
	}

	var hasUser bool

	var kept int

	for _, message := range request.messages {
		if message.Role == fantasy.MessageRoleUser {
			hasUser = true
		}

		kept++
	}

	if kept >= 1+2*40 {
		t.Fatal("nothing was trimmed; the test needs a smaller window to mean anything")
	}

	if !hasUser {
		t.Error("a trimmed thread lost its only user turn; strict providers reject the whole request")
	}
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

	run(t, &Options{
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

	if len(got) < len(want)+1 || got[len(got)-3] != want[0] || got[len(got)-2] != want[1] {
		t.Fatalf("the handler ran when only %v had been handed over", got)
	}

	if last := seenByHandler[len(seenByHandler)-1]; last.Activity == nil || last.Activity.Kind != conversation.ActivityRequest {
		t.Errorf("the request must be handed over with its turn, got %+v", last)
	}
}
