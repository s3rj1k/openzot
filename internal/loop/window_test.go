package loop_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/conversation"
	"github.com/openzot/openzot/internal/cycle"
	"github.com/openzot/openzot/internal/loop"
	"github.com/openzot/openzot/internal/outcome"
	"github.com/openzot/openzot/internal/testutils"
)

// fit runs the engine's fitter and returns the conversation it leaves, dropping the notices.
func fit(engine *loop.Engine, messages []conversation.Message, forgotten *int, turnStarts []int) []conversation.Message {
	grown, _ := engine.Fit.Fit(messages, forgotten, turnStarts, nil)

	return grown
}

// requestFor is what the engine would send for a conversation it has not seen
// before. Forgetting applied from scratch, then the request built. It also
// returns how many messages were forgotten.
func requestFor(engine *loop.Engine, messages []conversation.Message) (loop.TurnRequest, int) {
	forgotten := 0

	messages = fit(engine, messages, &forgotten, []int{len(messages)})

	return engine.BuildRequest(messages, forgotten), forgotten
}

// The heuristics must see what the engine actually records. A model repeating one
// call and getting one answer is a loop, however it words the turns between.
func TestTheEnginesOwnActivitiesTriggerCycleDetection(t *testing.T) {
	messages := make([]conversation.Message, 0, 8)

	for range 4 {
		messages = append(messages,
			testutils.Activity(conversation.ActivityRequest, "c1", "shell", `{"cmd":"ls"}`, nil),
			testutils.Activity(conversation.ActivityResponse, "c1", "shell", `{"cmd":"ls"}`, "same"),
		)
	}

	assert.NotEmpty(t, cycle.Describe(messages), "four identical call/result pairs must read as a cycle")

	// a different answer each time is progress
	polling := make([]conversation.Message, 0, 8)

	for _, answer := range []string{"a", "b", "c", "d"} {
		polling = append(polling,
			testutils.Activity(conversation.ActivityRequest, "c1", "shell", `{"cmd":"ls"}`, nil),
			testutils.Activity(conversation.ActivityResponse, "c1", "shell", `{"cmd":"ls"}`, answer),
		)
	}

	got := cycle.Describe(polling)
	assert.NotEqual(t, "repeated_result_run", got, "polling an endpoint until it changes is not a loop, got %q", got)
	assert.NotEqual(t, "repeated_activity_tail", got, "polling an endpoint until it changes is not a loop, got %q", got)
}

// Forgetting is a matter of the wire. A run long enough to fill a small window
// keeps every message in its conversation - what the session log records - and
// tells the viewer, not the model, what was dropped.
func TestALongRunKeepsEveryMessageAndSaysSo(t *testing.T) {
	var (
		notices int
		told    []string
	)

	engine, err := loop.New(&loop.Options{
		Model:         testutils.ScriptedModel(t, []string{testutils.Tool("c", litEcho, "{}")}),
		Tools:         echoTool(new(int)),
		ContextWindow: 3_000,
		MaxIterations: 60,
		MaxCycles:     100000,
		RetryBackoff:  -1,
		OnEvent: func(event loop.Event) {
			if event.Kind == loop.EventNotice && strings.Contains(event.Text, "forgot") {
				notices++
			}
		},
	})
	require.NoError(t, err)

	result := engine.Run(t.Context(), nil)

	require.Equal(t, outcome.StopIterations, result.Reason, "want the run to reach its iteration cap")

	assert.NotEqual(t, 0, notices, "the viewer was never told messages were forgotten")

	requests, responses := 0, 0

	for _, message := range result.Messages {
		if message.Activity != nil {
			switch message.Activity.Kind {
			case conversation.ActivityRequest:
				requests++
			case conversation.ActivityResponse:
				responses++
			default:
				// a trigger is neither half
			}
		}

		if strings.Contains(message.Text, "forgot") {
			told = append(told, message.Text)
		}
	}

	assert.Equal(t, 60, requests, "want all 60 of each")
	assert.Equal(t, 60, responses, "want all 60 of each")

	assert.Empty(t, told, "the model was told about forgetting")
}
