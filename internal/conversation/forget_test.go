package conversation

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// costs prices a message by the length of its text, so a test states its window
// in plain numbers.
func costs(message Message) int { return len(message.Text) }

// ten returns n messages costing ten each.
func ten(n int) []Message {
	messages := make([]Message, n)

	for i := range messages {
		messages[i] = Message{Type: TypeUser, Text: strings.Repeat("x", 10)}
	}

	return messages
}

// The window here is 1000 with the marks at 50% and 90%. 500 and 900.
func TestForget(t *testing.T) {
	tests := []struct {
		name string
		n    int // messages, ten each
		from int // already forgotten
		used int
		want int
	}{
		{"below the soft mark nothing goes", 40, 0, 499, 0},
		{"at the soft mark one goes", 40, 0, 500, 1},
		{"in the soft zone still only one", 40, 0, 899, 1},
		{"a forgotten offset carries on from where it was", 40, 5, 700, 6},
		{"at the hard mark it goes until under it", 40, 0, 950, 6},
		{"one drop that lands exactly on the hard mark is not enough", 40, 0, 910, 2},
		{"way over the hard mark goes on until under", 40, 0, 1100, 21},
		{"the floor caps it even when still over the hard mark", 40, 0, 1400, 38},
		{"the newest two are never forgotten", 4, 0, 5000, 2},
		{"a conversation shorter than the floor keeps everything", 2, 0, 5000, 0},
		{"an empty conversation", 0, 0, 5000, 0},
		{"an offset already past the floor does not move back", 4, 3, 5000, 3},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, Forget(ten(test.n), test.from, test.used, 1000, 50, 90, costs))
		})
	}
}

// One oversized newest message is kept, whatever it costs, and what came
// before it is what goes.
func TestForgetSpendsTheOldestFirstAndKeepsAnOversizedNewest(t *testing.T) {
	messages := append(ten(3), Message{Type: TypeUser, Text: strings.Repeat("y", 5000)})

	assert.Equal(t, 2, Forget(messages, 0, 5030, 1000, 50, 90, costs), "want the two oldest gone and the newest two kept")
}

// A tool call carries its cost outside the text. Priced by text alone, a request
// half would look free and a large write would slip past the window.
func TestMessageCostCountsTheToolCall(t *testing.T) {
	bare := Message{Type: TypeActivity}

	call := Message{Type: TypeActivity, Activity: &Activity{
		Kind: ActivityRequest, ID: "c1", Name: "write", Arguments: strings.Repeat("x", 900),
	}}

	assert.Greater(t, Cost(call), Cost(bare)+200, "a request's arguments must be priced: %d vs %d", Cost(call), Cost(bare))
}
