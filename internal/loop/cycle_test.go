package loop

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/openzot/openzot/internal/conversation"
)

// Cases the corpus cannot carry, since JSON cannot express a reference cycle. The question survives the port. A tool result
// that cannot be marshaled must not abort a run, because a cycle check that panics is worse than none.

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

	// Both cyclic results collapse to the same sentinel, so the two halves fingerprint the same and the
	// pair reads as a cycle. That is the intended trade in safeStringify. A degraded comparison beats none.
	assert.True(t, got, "want true (a cyclic result must degrade, not disable)")
}

func TestRepeatedResultRunCircularResult(t *testing.T) {
	messages := []conversation.Message{
		cycleResponse(cyclicValue()),
		cycleResponse(cyclicValue()),
		cycleResponse(cyclicValue()),
	}

	assert.True(t, hasRepeatedResultRun(messages), "want true (identical cyclic results are still a loop)")

	// a really different result must still break the run, even alongside a
	// cyclic one

	mixed := []conversation.Message{
		cycleResponse(cyclicValue()),
		cycleResponse(cyclicValue()),
		cycleResponse(map[string]any{"records": []any{"something"}}),
	}

	assert.False(t, hasRepeatedResultRun(mixed), "want false (a differing result breaks the run)")
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

	got, want := describeCycle(messages), "repeated_suffix"
	assert.Equal(t, want, got)

	assert.Empty(t, describeCycle(nil), "want empty for an empty conversation")
}
