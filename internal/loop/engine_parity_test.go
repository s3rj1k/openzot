package loop

import (
	"fmt"
	"github.com/openzot/openzot/internal/conversation"
	"strings"
	"testing"
)

// The cycle budget counts CONSECUTIVE cyclic rounds: a clean round in between
// must reset it, so two unrelated repetitions far apart in a long run do not add
// up to a false StopCycle. (Regressed once: the counter never reset.)
func TestCycleCounterResetsWhenACycleBreaks(t *testing.T) {
	engine, err := New(Options{ContextWindow: testWindow, Client: stub(t, []string{stop()})})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	budget := &Budget{}

	// [A B A B] - the last pair repeats the previous pair, which the repeated-suffix
	// heuristic flags as a cycle
	cyclic := []conversation.Message{
		{Type: conversation.TypeBot, Text: "let me try that"},
		{Type: conversation.TypeUser, Text: "ok"},
		{Type: conversation.TypeBot, Text: "let me try that"},
		{Type: conversation.TypeUser, Text: "ok"},
	}

	if next, stop := engine.checkCycle(cyclic, budget); stop != nil || next == nil {
		t.Fatal("a detected cycle must nudge (not stop yet, not ignore)")
	}

	if budget.Cycles != 1 {
		t.Fatalf("a detected cycle must be counted once, got %d", budget.Cycles)
	}

	// a clean, non-cyclic round breaks the run and must zero the counter
	clean := []conversation.Message{
		{Type: conversation.TypeUser, Text: "now do something different"},
		{Type: conversation.TypeBot, Text: "sure, here is a fresh approach"},
	}

	if next, stop := engine.checkCycle(clean, budget); stop != nil || next != nil {
		t.Fatal("a clean round must neither stop nor nudge")
	}

	if budget.Cycles != 0 {
		t.Errorf("a broken cycle must reset the counter to 0, got %d", budget.Cycles)
	}
}

// Forgetting must price a tool call by its whole payload, not just its text. A
// request half has no text - its cost is the name and arguments - so an
// argument-heavy call (writing a big file) must be counted, or a request the
// estimate thinks fits gets rejected by the provider.
func TestBuildRequestCountsToolCallArgumentsInTheWindow(t *testing.T) {
	// a window the huge call alone overflows, and the two recent turns fit in
	engine, err := New(Options{ContextWindow: 8000, Client: stub(t, []string{stop()})})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// varied text so BPE cannot merge it away - this must genuinely exceed the
	// window once counted
	huge := strings.Repeat("lorem ipsum dolor sit amet consectetur adipiscing ", 2000)
	args := fmt.Sprintf(`{"content":%q}`, huge)

	messages := []conversation.Message{
		request("big", "write", args),
		response("big", "write", args, "ok"),
		{Type: conversation.TypeUser, Text: "a short recent question"},
		{Type: conversation.TypeBot, Text: "a short recent answer"},
	}

	req, forgotten := requestFor(engine, messages)

	if forgotten == 0 {
		t.Error("the argument-heavy tool call was kept - its arguments were priced as empty, which is the bug")
	}

	for _, message := range req.messages {
		if call, ok := toolCallOf(message); ok && strings.Contains(call.Input, huge) {
			t.Error("the argument-heavy tool call reached the request")
		}
	}

	// the recent turns must survive (sanity: forgetting did keep something)
	var keptRecent bool
	for _, message := range req.messages {
		if strings.Contains(textOf(message), "short recent") {
			keptRecent = true
		}
	}
	if !keptRecent {
		t.Error("the recent turns must survive forgetting")
	}
}

// An empty turn stays bounded by the tight empty budget (a model
// producing nothing is stuck and must not burn the whole settle budget on
// silence), but its nudge points at the terminal tools so the model is told what
// settling actually requires - not the plain "say you are finished".
func TestSettleModeEmptyTurnIsBoundedButNudgesToSettle(t *testing.T) {
	// every turn is empty (no content, finish=stop) and the
	// empty budget is tighter than the settle budget
	result := run(t, Options{ContextWindow: testWindow,
		Client:     stub(t, []string{stop()}),
		MaxSettles: 5,
		MaxEmpties: 2,
	})

	// bounded by the empty budget, not the settle budget
	if result.Reason != StopEmpty {
		t.Errorf("repeated empty turns must stay bounded by the empty budget, got %q", result.Reason)
	}

	if result.Budget.Empties != 2 {
		t.Errorf("empties must reach max_empties (2), got %d", result.Budget.Empties)
	}

	// but the guidance must name the terminal tools (settle notice), not the plain
	// empty notice - proving the settle-aware nudge fired
	var sawTerminalGuidance bool
	for _, message := range result.Messages {
		if strings.Contains(message.Text, SuccessTool) {
			sawTerminalGuidance = true
		}
	}

	if !sawTerminalGuidance {
		t.Error("an empty turn's nudge must point at success/failure, not the plain empty notice")
	}
}

// The run accumulates the provider's own token counts (not the local estimate),
// so a viewer or the session summary can show real usage. Each call bills its
// whole prompt, so per-turn counts sum.
func TestRunAccumulatesProviderReportedUsage(t *testing.T) {
	client := stub(t, []string{settle("all done"), usageFrame(100, 40)})

	result := run(t, Options{ContextWindow: testWindow, Client: client})

	if result.Budget.InputTokens != 100 || result.Budget.OutputTokens != 40 {
		t.Errorf("run must accumulate provider usage, got in=%d out=%d",
			result.Budget.InputTokens, result.Budget.OutputTokens)
	}
}

// The empty budget counts CONSECUTIVE empty turns, as its own documentation says:
// a productive turn in between must reset it, so single stalls scattered over a
// long run do not add up to a false StopEmpty. (Regressed once: the counter was
// cumulative, so a run could die to its third stall hundreds of iterations after
// the first.)
func TestEmptyCounterResetsAfterAProductiveTurn(t *testing.T) {
	result := run(t, Options{ContextWindow: testWindow,
		Client: stub(t,
			[]string{stop()},                       // empty: 1/3
			[]string{tool("call_1", "echo", "{}")}, // productive - resets
			[]string{stop()},                       // empty: 1/3 again
			[]string{tool("call_2", "echo", "{}")}, // productive - resets
			[]string{settle("done")},               // settling ends the run
		),
		Tools:      echoTool(new(int)),
		Messages:   []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxEmpties: 3,
	})

	if result.Reason != StopSettled {
		t.Errorf("reason = %q, want %q - scattered empties must not stop the run", result.Reason, StopSettled)
	}

	if result.Budget.Empties != 0 {
		t.Errorf("empties = %d, want 0 - the last turns were productive", result.Budget.Empties)
	}
}
