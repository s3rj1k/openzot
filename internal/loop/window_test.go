package loop

import (
	"fmt"
	"strings"
	"testing"

	"github.com/openzot/openzot/internal/conversation"
)

// requestFor is what the engine would send for a conversation it has not seen
// before: forgetting applied from scratch, then the request built. It also
// returns how many messages were forgotten.
func requestFor(engine *Engine, messages []conversation.Message) (turnRequest, int) {
	forgotten := 0

	messages = engine.fitToWindow(messages, &forgotten, []int{len(messages)}, nil, func(Event) {})

	return engine.buildRequest(messages, forgotten), forgotten
}

// The heuristics must see what the engine actually records: a model repeating one
// call and getting one answer is a loop, however it words the turns in between.
func TestTheEnginesOwnActivitiesTriggerCycleDetection(t *testing.T) {
	var messages []conversation.Message

	for range 4 {
		messages = append(messages,
			activity(conversation.ActivityRequest, "c1", "shell", `{"cmd":"ls"}`, nil),
			activity(conversation.ActivityResponse, "c1", "shell", `{"cmd":"ls"}`, "same"),
		)
	}

	if got := describeCycle(messages); got == "" {
		t.Error("four identical call/result pairs must read as a cycle")
	}

	// a different answer each time is progress
	var polling []conversation.Message

	for _, answer := range []string{"a", "b", "c", "d"} {
		polling = append(polling,
			activity(conversation.ActivityRequest, "c1", "shell", `{"cmd":"ls"}`, nil),
			activity(conversation.ActivityResponse, "c1", "shell", `{"cmd":"ls"}`, answer),
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
		messages  = []conversation.Message{{Type: conversation.TypeUser, Text: "the kickoff"}}
		forgotten int
		firstLoss = -1
		hard      = window * DefaultContextHard / 100
	)

	for round := range 120 {
		id := fmt.Sprintf("c%d", round)

		messages = append(messages,
			conversation.Message{Type: conversation.TypeActivity, Activity: &conversation.Activity{Kind: conversation.ActivityRequest, ID: id, Name: "read", Arguments: `{"path":"x"}`}},
			conversation.Message{
				Type: conversation.TypeActivity, Text: strings.Repeat("line of file content ", 60),
				Activity: &conversation.Activity{Kind: conversation.ActivityResponse, ID: id, Name: "read", Result: strings.Repeat("line of file content ", 60)},
			},
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
		used := conversation.EstimateTokens(engine.instructions())

		for _, message := range messages[forgotten:] {
			used += conversation.Cost(message)
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

	result := engine.Run(t.Context(), nil)

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
			case conversation.ActivityRequest:
				requests++
			case conversation.ActivityResponse:
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
