package loop

import (
	"testing"

	"github.com/openzot/openzot/internal/conversation"
)

// Cases the corpus cannot carry. See notPortableCases in divergence_test.go for
// which captured records these stand in for.
//
// Both concern a value containing a reference cycle. JSON has no way to express
// one, so these could not be seeded - but the underlying question survives the
// port. A tool result that cannot be marshaled must not be the thing that aborts
// a run. A cycle check that panics is worse than no cycle check.

// cyclicValue returns a map that contains itself, which json.Marshal rejects.
func cyclicValue() map[string]any {
	value := map[string]any{"kind": "cyclic"}

	value["self"] = value

	return value
}

// cycleResponse is a tool result as the conversation holds it.
func cycleResponse(result any) conversation.Message {
	return conversation.Message{
		Type:     conversation.TypeActivity,
		Activity: &conversation.Activity{Kind: conversation.ActivityResponse, ID: "call", Name: "search", Arguments: `{"q":"same"}`, Result: result},
	}
}

func TestCycleCircularResult(t *testing.T) {
	cyclic := cycleResponse(cyclicValue())

	messages := []conversation.Message{
		{Type: conversation.TypeUser, Text: "A"},
		cyclic,
		{Type: conversation.TypeUser, Text: "A"},
		cyclic,
	}

	// must not panic, and must still reach a conclusion

	got := hasRepeatedSuffix(messages)

	/*
		@note both cyclic results collapse to the same sentinel, so the two halves
		fingerprint the same and the pair reads as a cycle. That is the
		intended trade-off in safeStringify. A degraded comparison beats none.
	*/
	if !got {
		t.Errorf("hasRepeatedSuffix = false, want true (a cyclic result must degrade, not disable)")
	}
}

func TestRepeatedResultRunCircularResult(t *testing.T) {
	messages := []conversation.Message{
		cycleResponse(cyclicValue()),
		cycleResponse(cyclicValue()),
		cycleResponse(cyclicValue()),
	}

	if !hasRepeatedResultRun(messages) {
		t.Error("hasRepeatedResultRun = false, want true (identical cyclic results are still a loop)")
	}

	// a really different result must still break the run, even alongside a
	// cyclic one

	mixed := []conversation.Message{
		cycleResponse(cyclicValue()),
		cycleResponse(cyclicValue()),
		cycleResponse(map[string]any{"records": []any{"something"}}),
	}

	if hasRepeatedResultRun(mixed) {
		t.Error("hasRepeatedResultRun = true, want false (a differing result breaks the run)")
	}
}

// TestDescribeAttributesTheHeuristic pins that attribution reports which check
// fired, not only that one did.
func TestDescribeAttributesTheHeuristic(t *testing.T) {
	messages := []conversation.Message{
		{Type: conversation.TypeUser, Text: "hello"},
		{Type: conversation.TypeBot, Text: "hi"},
		{Type: conversation.TypeUser, Text: "hello"},
		{Type: conversation.TypeBot, Text: "hi"},
	}

	if got, want := describeCycle(messages), "repeated_suffix"; got != want {
		t.Errorf("describeCycle = %q, want %q", got, want)
	}

	if got := describeCycle(nil); got != "" {
		t.Errorf("describeCycle = %q, want empty for an empty conversation", got)
	}
}
