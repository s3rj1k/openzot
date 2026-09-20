package loop

import (
	"context"
	"fmt"
	"strings"
	"testing"
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

// requestFor is what the engine would send for a conversation it has not seen
// before: forgetting applied from scratch, then the request built. It also
// returns how many messages were forgotten.
func requestFor(engine *Engine, messages []Message) (turnRequest, int) {
	forgotten := 0

	messages = engine.fitToWindow(messages, &forgotten, []int{len(messages)}, nil, func(Event) {})

	return engine.buildRequest(messages, forgotten), forgotten
}

// The window here is 1000 with the marks at 50% and 90%: 500 and 900.
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
			if got := forget(ten(test.n), test.from, test.used, 1000, 50, 90, costs); got != test.want {
				t.Errorf("forget = %d, want %d", got, test.want)
			}
		})
	}
}

// One oversized newest message is kept, whatever it costs, and what came
// before it is what goes.
func TestForgetSpendsTheOldestFirstAndKeepsAnOversizedNewest(t *testing.T) {
	messages := append(ten(3), Message{Type: TypeUser, Text: strings.Repeat("y", 5000)})

	if got := forget(messages, 0, 5030, 1000, 50, 90, costs); got != 2 {
		t.Errorf("forget = %d, want the two oldest gone and the newest two kept", got)
	}
}

// A tool call carries its cost outside the text. Priced by text alone, a request
// half would look free and a large write would slip past the window.
func TestMessageCostCountsTheToolCall(t *testing.T) {
	bare := Message{Type: TypeActivity}

	call := Message{Type: TypeActivity, Activity: &Activity{
		Kind: ActivityRequest, ID: "c1", Name: "write", Arguments: strings.Repeat("x", 900),
	}}

	if messageCost(call) <= messageCost(bare)+200 {
		t.Errorf("a request's arguments must be priced: %d vs %d", messageCost(call), messageCost(bare))
	}
}

// The heuristics must see what the engine actually records: a model repeating one
// call and getting one answer is a loop, however it words the turns in between.
func TestTheEnginesOwnActivitiesTriggerCycleDetection(t *testing.T) {
	var messages []Message

	for range 4 {
		messages = append(messages,
			activity(ActivityRequest, "c1", "shell", `{"cmd":"ls"}`, nil),
			activity(ActivityResponse, "c1", "shell", `{"cmd":"ls"}`, "same"),
		)
	}

	if got := describeCycle(messages); got == "" {
		t.Error("four identical call/result pairs must read as a cycle")
	}

	// a different answer each time is progress
	var polling []Message

	for _, answer := range []string{"a", "b", "c", "d"} {
		polling = append(polling,
			activity(ActivityRequest, "c1", "shell", `{"cmd":"ls"}`, nil),
			activity(ActivityResponse, "c1", "shell", `{"cmd":"ls"}`, answer),
		)
	}

	if got := describeCycle(polling); got == "repeated_result_run" || got == "repeated_activity_tail" {
		t.Errorf("polling an endpoint until it changes is not a loop, got %q", got)
	}
}

// A conversation growing round by round: nothing is forgotten until the soft
// mark, then the request loses one message per round, and it never reaches the
// hard mark. The conversation handed in is never touched.
func TestARequestNeverReachesTheHardMark(t *testing.T) {
	const window = 20_000

	engine, err := New(Options{Client: stub(t, []string{stop()}), ContextWindow: window})
	if err != nil {
		t.Fatal(err)
	}

	var (
		messages  = []Message{{Type: TypeUser, Text: "the kickoff"}}
		forgotten int
		firstLoss = -1
		hard      = window * DefaultContextHard / 100
	)

	for round := 0; round < 120; round++ {
		id := fmt.Sprintf("c%d", round)

		messages = append(messages,
			Message{Type: TypeActivity, Activity: &Activity{Kind: ActivityRequest, ID: id, Name: "read", Arguments: `{"path":"x"}`}},
			Message{Type: TypeActivity, Text: strings.Repeat("line of file content ", 60),
				Activity: &Activity{Kind: ActivityResponse, ID: id, Name: "read", Result: strings.Repeat("line of file content ", 60)}},
		)

		before := forgotten

		messages = engine.fitToWindow(messages, &forgotten, []int{len(messages)}, nil, func(Event) {})

		if forgotten < before {
			t.Fatalf("round %d: the offset moved back from %d to %d", round, before, forgotten)
		}

		if forgotten > before && firstLoss < 0 {
			firstLoss = round
		}

		// what the request carries is what is left after the offset
		used := estimateTokens(engine.instructions())

		for _, message := range messages[forgotten:] {
			used += messageCost(message)
		}

		if used >= hard {
			t.Fatalf("round %d: the request costs %d, at or past the hard mark %d", round, used, hard)
		}
	}

	if firstLoss < 0 {
		t.Fatal("nothing was ever forgotten")
	}

	if len(messages) != 1+2*120 {
		t.Errorf("the conversation was rewritten: %d messages", len(messages))
	}
}

// Forgetting is a matter of the wire: a run long enough to fill a small window
// keeps every message in its conversation - what the session log records - and
// tells the viewer, not the model, what was dropped.
func TestALongRunKeepsEveryMessageAndSaysSo(t *testing.T) {
	var (
		notices int
		told    []string
	)

	engine, err := New(Options{
		Client:        stub(t, []string{tool("c", "echo", "{}")}),
		Tools:         echoTool(new(int)),
		ContextWindow: 3_000,
		MaxIterations: 60,
		MaxCycles:     100000,
		RetryBackoff:  -1,
		OnEvent: func(event Event) {
			if event.Kind == EventNotice && strings.Contains(event.Text, "forgot") {
				notices++
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	result := engine.Run(context.Background(), nil)

	if result.Reason != StopIterations {
		t.Fatalf("reason = %q, want the run to reach its iteration cap", result.Reason)
	}

	if notices == 0 {
		t.Error("the viewer was never told messages were forgotten")
	}

	requests, responses := 0, 0

	for _, message := range result.Messages {
		if message.Activity != nil {
			switch message.Activity.Kind {
			case ActivityRequest:
				requests++
			case ActivityResponse:
				responses++
			}
		}

		if strings.Contains(message.Text, "forgot") {
			told = append(told, message.Text)
		}
	}

	if requests != 60 || responses != 60 {
		t.Errorf("the conversation holds %d requests and %d responses, want all 60 of each", requests, responses)
	}

	if len(told) != 0 {
		t.Errorf("the model was told about forgetting: %q", told)
	}
}
