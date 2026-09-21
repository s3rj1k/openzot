package loop_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/conversation"
	"github.com/openzot/openzot/internal/loop"
	"github.com/openzot/openzot/internal/testutils"
)

// requestFor is what the engine would send for a conversation it has not seen
// before. Forgetting applied from scratch, then the request built. It also
// returns how many messages were forgotten.
func requestFor(engine *loop.Engine, messages []conversation.Message) (loop.TurnRequest, int) {
	forgotten := 0

	messages = engine.FitToWindow(messages, &forgotten, []int{len(messages)}, nil, func(loop.Event) {})

	return engine.BuildRequest(messages, forgotten), forgotten
}

// The heuristics must see what the engine actually records. A model repeating one
// call and getting one answer is a loop, however it words the turns between.
func TestTheEnginesOwnActivitiesTriggerCycleDetection(t *testing.T) {
	messages := make([]conversation.Message, 0, 8)

	for range 4 {
		messages = append(messages,
			activity(conversation.ActivityRequest, "c1", "shell", `{"cmd":"ls"}`, nil),
			activity(conversation.ActivityResponse, "c1", "shell", `{"cmd":"ls"}`, "same"),
		)
	}

	assert.NotEmpty(t, loop.DescribeCycle(messages), "four identical call/result pairs must read as a cycle")

	// a different answer each time is progress
	polling := make([]conversation.Message, 0, 8)

	for _, answer := range []string{"a", "b", "c", "d"} {
		polling = append(polling,
			activity(conversation.ActivityRequest, "c1", "shell", `{"cmd":"ls"}`, nil),
			activity(conversation.ActivityResponse, "c1", "shell", `{"cmd":"ls"}`, answer),
		)
	}

	got := loop.DescribeCycle(polling)
	assert.NotEqual(t, "repeated_result_run", got, "polling an endpoint until it changes is not a loop, got %q", got)
	assert.NotEqual(t, "repeated_activity_tail", got, "polling an endpoint until it changes is not a loop, got %q", got)
}

// A conversation growing round by round. Nothing is forgotten until the soft
// mark, then the request loses one message per round, and it never reaches the
// hard mark. The conversation handed in is never touched.
func TestARequestNeverReachesTheHardMark(t *testing.T) {
	const window = 20_000

	engine, err := loop.New(&loop.Options{Client: testutils.ScriptedClient(t, []string{testutils.Stop()}), ContextWindow: window})
	require.NoError(t, err)

	var (
		messages  = []conversation.Message{{Type: conversation.TypeUser, Text: "the kickoff"}}
		forgotten int
		firstLoss = -1
		hard      = window * loop.DefaultContextHard / 100
	)

	for round := range 120 {
		id := fmt.Sprintf("c%d", round)

		messages = append(messages,
			conversation.Message{Type: conversation.TypeActivity, Activity: &conversation.Activity{Kind: conversation.ActivityRequest, ID: id, Name: litRead, Arguments: `{"path":"x"}`}},
			conversation.Message{
				Type: conversation.TypeActivity, Text: strings.Repeat("line of file content ", 60),
				Activity: &conversation.Activity{Kind: conversation.ActivityResponse, ID: id, Name: litRead, Result: strings.Repeat("line of file content ", 60)},
			},
		)

		before := forgotten

		messages = engine.FitToWindow(messages, &forgotten, []int{len(messages)}, nil, func(loop.Event) {})

		require.GreaterOrEqual(t, forgotten, before, "round %d: the offset moved back from %d to %d", round, before, forgotten)

		if forgotten > before && firstLoss < 0 {
			firstLoss = round
		}

		// what the request carries is what is left after the offset
		used := conversation.EstimateTokens(engine.Options.Instructions)

		for _, message := range messages[forgotten:] {
			used += conversation.Cost(message)
		}

		require.Less(t, used, hard, "round %d: the request costs %d, at or past the hard mark %d", round, used, hard)
	}

	require.GreaterOrEqual(t, firstLoss, 0, "nothing was ever forgotten")

	assert.Len(t, messages, 1+2*120)
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
		Client:        testutils.ScriptedClient(t, []string{testutils.Tool("c", litEcho, "{}")}),
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

	require.Equal(t, loop.StopIterations, result.Reason, "want the run to reach its iteration cap")

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
