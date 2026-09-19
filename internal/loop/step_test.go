package loop

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"charm.land/fantasy"
)

// toolCalls is one model turn asking for several tools at once, finishing with
// the given reason.
func toolCalls(finish string, calls ...[3]string) string {
	parts := make([]string, len(calls))

	for i, call := range calls {
		parts[i] = fmt.Sprintf(
			`{"index":%d,"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}`,
			i, call[0], call[1], call[2])
	}

	return `{"choices":[{"delta":{"tool_calls":[` + strings.Join(parts, ",") +
		`]},"finish_reason":` + fmt.Sprintf("%q", finish) + `}]}`
}

func countTool(calls *int, name string) fantasy.AgentTool {
	return namedTool(name, func(context.Context) (any, error) {
		*calls++

		return "ok", nil
	})
}

// A terminal call ends the run before anything else of its turn is acted on: a
// shell command in the same turn as "success" is not run, and leaves nothing in
// the conversation.
func TestATerminalCallEndsTheRunBeforeItsSiblingsRun(t *testing.T) {
	ran := 0

	result := run(t, Options{ContextWindow: testWindow,
		Client: stub(t, []string{toolCalls("tool_calls",
			[3]string{"c1", "echo", `{}`},
			[3]string{"c2", SuccessTool, `{"summary":"all done"}`},
		)}),
		Tools:      []fantasy.AgentTool{countTool(&ran, "echo")},
		Messages:   []Message{{Type: TypeUser, Text: "go"}},
		MaxSettles: 5,
	})

	if result.Reason != StopSettled || result.Message != "all done" {
		t.Errorf("reason = %q, message = %q, want settled with the summary", result.Reason, result.Message)
	}

	if ran != 0 {
		t.Errorf("the sibling tool ran %d times, want it not to run at all", ran)
	}

	if requests, responses := countActivities(result.Messages); requests != 0 || responses != 0 {
		t.Errorf("the conversation holds %d requests and %d responses, want none", requests, responses)
	}
}

// The call that would overrun the budget is not made, and does not leave a
// request with no answer behind: the next request would be invalid.
func TestTheCallBudgetStopsBeforeTheCallThatOverrunsIt(t *testing.T) {
	ran := 0

	result := run(t, Options{ContextWindow: testWindow,
		Client: stub(t, []string{toolCalls("tool_calls",
			[3]string{"c1", "echo", `{}`},
			[3]string{"c2", "echo", `{}`},
		)}),
		Tools:    []fantasy.AgentTool{countTool(&ran, "echo")},
		Messages: []Message{{Type: TypeUser, Text: "go"}},
		MaxCalls: 1,
	})

	if result.Reason != StopCalls {
		t.Fatalf("reason = %q, want the call budget to stop the run", result.Reason)
	}

	if ran != 1 {
		t.Errorf("the tool ran %d times, want just the one call within the budget", ran)
	}

	if requests, responses := countActivities(result.Messages); requests != 1 || responses != 1 {
		t.Errorf("the conversation holds %d requests and %d responses, want one answered pair", requests, responses)
	}
}

// Endpoints in the wild end a turn that carries tool calls with "stop". The calls
// are what the model asked for and they are run.
func TestToolCallsAreRunWhateverTheProviderCalledTheEnding(t *testing.T) {
	for _, finish := range []string{"stop", "something_new"} {
		ran := 0

		result := run(t, Options{ContextWindow: testWindow,
			Client: stub(t,
				[]string{toolCalls(finish, [3]string{"c1", "echo", `{}`})},
				[]string{text("done"), stop()},
			),
			Tools:         []fantasy.AgentTool{countTool(&ran, "echo")},
			Messages:      []Message{{Type: TypeUser, Text: "go"}},
			MaxIterations: 5,
		})

		if ran != 1 {
			t.Errorf("finish %q: the tool ran %d times, want 1", finish, ran)
		}

		if result.Reason != StopStop {
			t.Errorf("finish %q: reason = %q, want the run to carry on and stop normally", finish, result.Reason)
		}
	}
}

// A turn cut off at the output limit can carry a call whose arguments were cut
// off with it. It is never run; the model is asked to continue.
func TestACallFromATruncatedTurnIsNeverRun(t *testing.T) {
	ran := 0

	result := run(t, Options{ContextWindow: testWindow,
		Client: stub(t,
			[]string{toolCalls("length", [3]string{"c1", "echo", `{}`})},
			[]string{text("done"), stop()},
		),
		Tools:         []fantasy.AgentTool{countTool(&ran, "echo")},
		Messages:      []Message{{Type: TypeUser, Text: "go"}},
		MaxIterations: 5,
	})

	if ran != 0 {
		t.Errorf("the tool ran %d times, want the cut-off call not to run", ran)
	}

	if result.Budget.Continuations == 0 && result.Budget.Recoveries == 0 {
		t.Error("a truncated turn must be continued, not accepted")
	}
}

// The engine never leaves a conversation ending on the model's own words, but
// one it is handed might; fantasy will not start from it, so it is given a line
// to continue from rather than failing the run.
func TestAConversationEndingOnTheModelsWordsStillRuns(t *testing.T) {
	result := run(t, Options{ContextWindow: testWindow,
		Client: stub(t, []string{text("carrying on"), stop()}),
		Messages: []Message{
			{Type: TypeUser, Text: "go"},
			{Type: TypeBot, Text: "I began"},
		},
	})

	if result.Reason != StopStop {
		t.Fatalf("reason = %q, err = %v, want the run to go ahead", result.Reason, result.Err)
	}
}

// A call to a tool that does not exist, or with input that cannot be read, is
// answered by fantasy without the tool being touched. It is still a call: it
// counts, and it is written into the conversation as a request and a failure.
func TestACallThatNeverReachedATool(t *testing.T) {
	result := run(t, Options{ContextWindow: testWindow,
		Client: stub(t,
			[]string{toolCalls("tool_calls", [3]string{"c1", "missing", `{}`})},
			[]string{text("noted"), stop()},
		),
		Messages:      []Message{{Type: TypeUser, Text: "go"}},
		MaxIterations: 5,
	})

	if result.Budget.Calls != 1 {
		t.Errorf("calls = %d, want the refused call counted", result.Budget.Calls)
	}

	if requests, responses := countActivities(result.Messages); requests != 1 || responses != 1 {
		t.Errorf("the conversation holds %d requests and %d responses, want one pair", requests, responses)
	}

	if !mentionsAFailure(result.Messages) {
		t.Error("the refusal must be written down as a failure")
	}
}

// A tool the engine was told never to repair is given no input the model did not
// finish writing: a command cut off mid-string goes back to the model, and does
// not run. The same slip in an ordinary tool is mended and the tool runs.
func TestAToolThatIsNeverRepairedRefusesAnUnfinishedCall(t *testing.T) {
	unfinished := `{"value": "rm -rf build`

	for _, test := range []struct {
		name       string
		unrepaired []string
		wantRan    int
	}{
		{"a listed tool is refused", []string{"echo"}, 0},
		{"an unlisted tool is repaired and run", nil, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			ran := 0

			result := run(t, Options{ContextWindow: testWindow,
				Client: stub(t,
					[]string{toolCalls("tool_calls", [3]string{"c1", "echo", unfinished})},
					[]string{text("done"), stop()},
				),
				Tools:         []fantasy.AgentTool{countTool(&ran, "echo")},
				Unrepaired:    test.unrepaired,
				Messages:      []Message{{Type: TypeUser, Text: "go"}},
				MaxIterations: 5,
			})

			if ran != test.wantRan {
				t.Errorf("the tool ran %d times, want %d", ran, test.wantRan)
			}

			if refused := mentionsAFailure(result.Messages); refused != (test.wantRan == 0) {
				t.Errorf("a failure was recorded = %v, want %v", refused, test.wantRan == 0)
			}
		})
	}
}
