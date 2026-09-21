package loop_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3rj1k/agent/internal/conversation"
	"github.com/s3rj1k/agent/internal/loop"
	"github.com/s3rj1k/agent/internal/outcome"
	"github.com/s3rj1k/agent/internal/testutils"
)

// The cycle budget counts CONSECUTIVE cyclic rounds. A clean round between
// must reset it, so two unrelated repetitions far apart in a long run do not add
// up to a false StopCycle. (Regressed once. The counter never reset.)
func TestCycleCounterResetsWhenACycleBreaks(t *testing.T) {
	engine, err := loop.New(&loop.Options{ContextWindow: testWindow, Model: testutils.ScriptedModel(t, []string{testutils.Stop()})})
	require.NoError(t, err)

	budget := &outcome.Budget{}

	// [A B A B] - the last pair repeats the previous pair, which the repeated-suffix
	// heuristic flags as a cycle
	cyclic := []conversation.Message{
		{Type: conversation.TypeBot, Text: "let me try that"},
		{Type: conversation.TypeUser, Text: "ok"},
		{Type: conversation.TypeBot, Text: "let me try that"},
		{Type: conversation.TypeUser, Text: "ok"},
	}

	next, stop := engine.CheckCycle(cyclic, budget)
	require.Nil(t, stop, "a detected cycle must nudge (not stop yet, not ignore)")
	require.NotNil(t, next, "a detected cycle must nudge (not stop yet, not ignore)")

	require.Equal(t, 1, budget.Cycles, "a detected cycle must be counted once, got %d", budget.Cycles)

	// a clean, non-cyclic round breaks the run and must zero the counter
	clean := []conversation.Message{
		{Type: conversation.TypeUser, Text: "now do something different"},
		{Type: conversation.TypeBot, Text: "sure, here is a fresh approach"},
	}

	next, stop = engine.CheckCycle(clean, budget)
	require.Nil(t, stop, "a clean round must neither stop nor nudge")
	require.Nil(t, next, "a clean round must neither stop nor nudge")

	assert.Equal(t, 0, budget.Cycles, "a broken cycle must reset the counter to 0, got %d", budget.Cycles)
}

// Forgetting must price a tool call by its whole payload, not just its text. A request half has no text, so an argument-heavy
// call (writing a big file) must be counted, or a request the estimate thinks fits gets rejected by the provider.
func TestBuildRequestCountsToolCallArgumentsInTheWindow(t *testing.T) {
	// a window the huge call alone overflows, and the two recent turns fit in
	engine, err := loop.New(&loop.Options{ContextWindow: 8000, Model: testutils.ScriptedModel(t, []string{testutils.Stop()})})
	require.NoError(t, err)

	// varied text so BPE cannot merge it away - this must really exceed the
	// Window once counted
	huge := strings.Repeat("lorem ipsum dolor sit amet consectetur adipiscing ", 2000)
	args := fmt.Sprintf(`{"content":%q}`, huge)

	messages := []conversation.Message{
		testutils.Request("big", "write", args),
		testutils.Response("big", "write", args, "ok"),
		{Type: conversation.TypeUser, Text: "a short recent question"},
		{Type: conversation.TypeBot, Text: "a short recent answer"},
	}

	req, forgotten := requestFor(engine, messages)

	assert.NotEqual(t, 0, forgotten, "the argument-heavy tool call was kept - its arguments were priced as empty, which is the bug")

	for _, message := range req.Messages {
		if call, ok := testutils.ToolCallOf(message); ok {
			assert.NotContains(t, call.Input, huge, "the argument-heavy tool call reached the request")
		}
	}

	// the recent turns must survive (sanity. Forgetting did keep something)
	var keptRecent bool

	for _, message := range req.Messages {
		if strings.Contains(testutils.TextOf(message), "short recent") {
			keptRecent = true
		}
	}

	assert.True(t, keptRecent, "the recent turns must survive forgetting")
}

// An empty turn stays bounded by the tight empty budget, since a model producing nothing is stuck and must not burn the settle
// budget on silence. Its nudge points at the terminal tools, so the model is told what settling requires.
func TestSettleModeEmptyTurnIsBoundedButNudgesToSettle(t *testing.T) {
	// every turn is empty (no content, finish=stop) and the
	// empty budget is tighter than the settle budget
	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Model:         testutils.ScriptedModel(t, []string{testutils.Stop()}),
		MaxSettles:    5,
		MaxEmpties:    2,
	})

	// bounded by the empty budget, not the settle budget
	assert.Equal(t, outcome.StopEmpty, result.Reason, "repeated empty turns must stay bounded by the empty budget, got %q", result.Reason)

	assert.Equal(t, 2, result.Budget.Empties)

	// but the guidance must name the terminal tools (settle notice), not the plain
	// empty notice - proving the settle-aware nudge fired
	var sawTerminalGuidance bool

	for _, message := range result.Messages {
		if strings.Contains(message.Text, outcome.SuccessTool) {
			sawTerminalGuidance = true
		}
	}

	assert.True(t, sawTerminalGuidance, "an empty turn's nudge must point at success/failure, not the plain empty notice")
}

// The run accumulates the provider's own token counts (not the local estimate),
// so a viewer or the session summary can show real usage. Each call bills its
// whole prompt, so per-turn counts sum.
func TestRunAccumulatesProviderReportedUsage(t *testing.T) {
	client := testutils.ScriptedModel(t, []string{testutils.Settle("all done"), testutils.Usage(100, 40)})

	result := run(t, &loop.Options{ContextWindow: testWindow, Model: client})

	assert.Equal(t, 100, result.Budget.InputTokens, "run must accumulate provider usage, got in=%d out=%d", result.Budget.InputTokens, result.Budget.OutputTokens)
	assert.Equal(t, 40, result.Budget.OutputTokens, "run must accumulate provider usage, got in=%d out=%d", result.Budget.InputTokens, result.Budget.OutputTokens)
}

// The empty budget counts consecutive empty turns, so a productive turn between must reset it and scattered stalls do not
// add up to a false StopEmpty. It regressed once, when the counter was cumulative and a run died to its third stall.
func TestEmptyCounterResetsAfterAProductiveTurn(t *testing.T) {
	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Model: testutils.ScriptedModel(t,
			[]string{testutils.Stop()},                        // empty. 1/3
			[]string{testutils.Tool("call_1", litEcho, "{}")}, // productive - resets
			[]string{testutils.Stop()},                        // empty. 1/3 again
			[]string{testutils.Tool("call_2", litEcho, "{}")}, // productive - resets
			[]string{testutils.Settle("done")},                // settling ends the run
		),
		Tools:      echoTool(new(int)),
		Messages:   []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxEmpties: 3,
	})

	assert.Equal(t, outcome.StopSettled, result.Reason, "reason = %q, want %q - scattered empties must not stop the run", result.Reason, outcome.StopSettled)

	assert.Equal(t, 0, result.Budget.Empties, "want 0 - the last turns were productive")
}
