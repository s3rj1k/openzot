package loop_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/conversation"
	"github.com/openzot/openzot/internal/loop"
)

const planArgs = `{"tasks":[{"title":"read the code","status":"done"},{"title":"fix it","status":"in_progress"}]}`

// planCall is the model calling its plan tool and being answered.
func planCall(id, args, answer string) []conversation.Message {
	return []conversation.Message{
		activity(conversation.ActivityRequest, id, litTasks, args, nil),
		activity(conversation.ActivityResponse, id, litTasks, args, answer),
	}
}

func planEngine(t *testing.T, options *loop.Options) *loop.Engine {
	t.Helper()

	options.Client = stub(t, []string{stop()})

	if options.ContextWindow == 0 {
		options.ContextWindow = testWindow
	}

	options.PlanTool = litTasks

	engine, err := loop.New(options)
	require.NoError(t, err)

	return engine
}

// The plan is the model's latest successful call of the plan tool, said again
// under a fresh id - and only when it has been forgotten.
func TestRepostedPlan(t *testing.T) {
	engine := planEngine(t, &loop.Options{})

	messages := append(planCall("a", `{"tasks":[]}`, "old plan"), planCall("b", planArgs, "the plan")...)
	messages = append(messages, conversation.Message{Type: conversation.TypeUser, Text: litLater})

	t.Run("a plan that fell out of the window is posted again", func(t *testing.T) {
		posted, ok := engine.RepostedPlan(messages, 5)
		require.True(t, ok, "want the call and its result")
		require.Len(t, posted, 2, "want the call and its result")

		call, result := posted[0].Activity, posted[1].Activity

		assert.Equal(t, conversation.ActivityRequest, call.Kind)
		assert.Equal(t, conversation.ActivityResponse, result.Kind)
		assert.Equal(t, result.ID, call.ID)

		assert.NotEqual(t, "a", call.ID, "the id %q is one the model already used", call.ID)
		assert.NotEqual(t, "b", call.ID, "the id %q is one the model already used", call.ID)

		assert.Equal(t, litTasks, call.Name)
		assert.JSONEq(t, planArgs, call.Arguments)
		assert.Equal(t, "the plan", result.Result)
		assert.Equal(t, "the plan", posted[1].Text)
	})

	t.Run("a plan still in the window needs no help", func(t *testing.T) {
		// the plan's result is message 3. At the offset it is the oldest one held
		for _, forgotten := range []int{2, 3} {
			_, ok := engine.RepostedPlan(messages, forgotten)
			assert.False(t, ok, "the plan was posted while its result is still in the window (offset %d)", forgotten)
		}

		_, ok := engine.RepostedPlan(messages, 4)
		assert.True(t, ok, "the plan was not posted with its result one message out of the window")
	})

	t.Run("no plan, nothing to post", func(t *testing.T) {
		_, ok := engine.RepostedPlan([]conversation.Message{{Type: conversation.TypeUser, Text: "hi"}}, 1)
		assert.False(t, ok)
	})

	t.Run("a refused call is not the plan", func(t *testing.T) {
		refused := append([]conversation.Message(nil), messages[:4]...)
		refused = append(refused,
			activity(conversation.ActivityRequest, "c", litTasks, `{"tasks":[]}`, nil),
			conversation.Message{Type: conversation.TypeActivity, Activity: &conversation.Activity{Kind: conversation.ActivityResponse, ID: "c", Name: litTasks, Arguments: `{"tasks":[]}`, Failure: "tasks needs at least one task"}},
			conversation.Message{Type: conversation.TypeUser, Text: litLater},
		)

		posted, ok := engine.RepostedPlan(refused, 6)
		assert.True(t, ok, "the plan should be the last one that worked, got %+v (ok=%v)", posted, ok)
		assert.JSONEq(t, planArgs, posted[0].Activity.Arguments, "want the last plan that worked")
	})

	t.Run("another tool is not the plan", func(t *testing.T) {
		other := []conversation.Message{activity(conversation.ActivityResponse, "x", "shell", "{}", "out"), {Type: conversation.TypeUser, Text: litLater}}

		_, ok := engine.RepostedPlan(other, 1)
		assert.False(t, ok, "a shell result was taken for the plan")
	})

	t.Run("without a plan tool there is no plan", func(t *testing.T) {
		bare, err := loop.New(&loop.Options{Client: stub(t, []string{stop()}), ContextWindow: testWindow})
		require.NoError(t, err)

		_, ok := bare.RepostedPlan(messages, 5)
		assert.False(t, ok, "a plan was posted with no plan tool")
	})
}

// history is a plan, then n turns of tool work each costing about cost tokens.
func history(n int, filler string) []conversation.Message {
	messages := make([]conversation.Message, 0, 3+2*n)
	messages = append(messages, conversation.Message{Type: conversation.TypeUser, Text: "kickoff"})
	messages = append(messages, planCall("plan", planArgs, "the plan")...)

	for i := range n {
		id := fmt.Sprintf("c%d", i)

		messages = append(messages,
			activity(conversation.ActivityRequest, id, litRead, `{"path":"x"}`, nil),
			activity(conversation.ActivityResponse, id, litRead, `{"path":"x"}`, filler),
		)
	}

	return messages
}

func starts(messages []conversation.Message) []int {
	// the plan is turn one, every pair after it another, and the next is coming
	out := []int{0, 1}

	for i := 3; i < len(messages); i += 2 {
		out = append(out, i)
	}

	return append(out, len(messages))
}

// After forgetting, a window with too little left in it gets the plan back - as
// the newest thing in it, so it is not forgotten again straight away.
func TestForgettingLeavesTooFewTurnsSoThePlanIsPosted(t *testing.T) {
	engine := planEngine(t, &loop.Options{ContextWindow: 4_000, PlanMinTurns: 5})

	messages := history(12, strings.Repeat("file content ", 60))
	forgotten := 0

	grown := engine.FitToWindow(messages, &forgotten, starts(messages), nil, func(loop.Event) {})

	require.Len(t, grown, len(messages)+2, "the conversation grew by %d messages, want the 2 of the plan", len(grown)-len(messages))

	request := engine.BuildRequest(grown, forgotten)

	var posted bool

	for _, message := range request.Messages {
		if call, ok := toolCallOf(message); ok && call.ToolName == litTasks && call.Input == planArgs {
			posted = true
		}
	}

	assert.True(t, posted)
}

// A turn is whole only if none of it was forgotten, and the one being asked for
// has not happened yet.
func TestTurnsHeld(t *testing.T) {
	starts := []int{0, 4, 8, 12, 16}

	for forgotten, want := range map[int]int{0: 4, 4: 3, 5: 2, 8: 2, 13: 0, 16: 0, 40: 0} {
		got := loop.TurnsHeld(starts, forgotten)
		assert.Equal(t, want, got, "offset %d holds %d turns, want %d", forgotten, got, want)
	}
}

// The plan goes back when the window holds fewer turns than plan_min_turns, and
// not when it holds exactly that many.
func TestThePlanGoesBackOnlyBelowTheMinimumTurns(t *testing.T) {
	messages := history(12, strings.Repeat("file content ", 60))
	marks := starts(messages)

	// what the window holds when the decision is made. After forgetting, before
	// the plan is put back
	probe := planEngine(t, &loop.Options{ContextWindow: 4_000})
	forgotten := 0
	probe.ForgetOldest(messages, &forgotten, nil, func(loop.Event) {})

	held := loop.TurnsHeld(marks, forgotten)
	require.NotEqual(t, 0, held, "test setup: no turns held")

	for min, wantPosted := range map[int]bool{held - 1: false, held: false, held + 1: true} {
		engine := planEngine(t, &loop.Options{ContextWindow: 4_000, PlanMinTurns: min})
		offset := 0

		grown := engine.FitToWindow(messages, &offset, marks, nil, func(loop.Event) {})

		posted := len(grown) > len(messages)
		assert.Equal(t, wantPosted, posted, "with %d turns held and a minimum of %d, posted = %v, want %v", held, min, posted, wantPosted)
	}
}

func TestThePlanIsLeftAloneWhenTheWindowStillHoldsEnoughTurns(t *testing.T) {
	engine := planEngine(t, &loop.Options{ContextWindow: 4_000, PlanMinTurns: 2})

	messages := history(12, strings.Repeat("file content ", 60))
	forgotten := 0

	grown := engine.FitToWindow(messages, &forgotten, starts(messages), nil, func(loop.Event) {})

	require.NotEqual(t, 0, forgotten, "test setup: nothing was forgotten")

	assert.Len(t, grown, len(messages), "the plan was posted with plenty of turns left (%d messages added)", len(grown)-len(messages))
}

func TestNothingIsPostedWhileNothingIsForgotten(t *testing.T) {
	engine := planEngine(t, &loop.Options{PlanMinTurns: 100})

	messages := history(3, "short")
	forgotten := 0

	assert.Len(t, engine.FitToWindow(messages, &forgotten, starts(messages), nil, func(loop.Event) {}), len(messages), "the plan was posted though the whole conversation fits")
}

func TestThePlanIsNotPostedTwice(t *testing.T) {
	engine := planEngine(t, &loop.Options{ContextWindow: 4_000, PlanMinTurns: 100})

	messages := history(12, strings.Repeat("file content ", 60))
	forgotten := 0

	messages = engine.FitToWindow(messages, &forgotten, starts(messages), nil, func(loop.Event) {})
	size := len(messages)

	// the next request forgets again, but the posted plan is well inside the window
	messages = append(messages,
		activity(conversation.ActivityRequest, "n", litRead, `{"path":"y"}`, nil),
		activity(conversation.ActivityResponse, "n", litRead, `{"path":"y"}`, strings.Repeat("file content ", 60)),
	)

	messages = engine.FitToWindow(messages, &forgotten, append(starts(messages[:size]), size, len(messages)), nil, func(loop.Event) {})

	assert.Len(t, messages, size+2, "the plan was posted again while the last copy is in the window (%d extra messages)", len(messages)-size-2)
}

// planRun runs an engine that lays out a plan and then keeps working, and
// returns the conversation.
func planRun(t *testing.T, options *loop.Options, iterations int) *loop.Result {
	t.Helper()

	options.Client = stub(t,
		[]string{tool("p", litTasks, planArgs)},
		[]string{tool("c", litEcho, "{}")},
	)

	options.Tools = append(echoTool(new(int)), namedTool(litTasks, func(context.Context) (any, error) { return "tasks: 0/2 done", nil }))
	options.MaxIterations = iterations
	options.MaxCycles = 100000
	options.RetryBackoff = -1

	if options.ContextWindow == 0 {
		options.ContextWindow = testWindow
	}

	engine, err := loop.New(options)
	require.NoError(t, err)

	result := engine.Run(t.Context(), nil)

	return &result
}

func countNudges(result *loop.Result) int {
	nudges := 0

	for _, message := range result.Messages {
		if message.Type == conversation.TypeUser && strings.Contains(message.Text, loop.PlanNudge(litTasks)) {
			nudges++
		}
	}

	return nudges
}

func TestThePlanToolIsRememberedEveryNthIteration(t *testing.T) {
	// iterations 3, 6 and 9 of ten
	assert.Equal(t, 3, countNudges(planRun(t, &loop.Options{PlanTool: litTasks, PlanNudgeEvery: 3}, 10)))

	// the default is every fifth
	got := countNudges(planRun(t, &loop.Options{PlanTool: litTasks}, 11))
	assert.Equal(t, 2, got, "nudged %d times with the default, want 2 (iterations 5 and 10)", got)
}

func TestPlanRemindersCanBeSwitchedOffAndNeedAPlanTool(t *testing.T) {
	got := countNudges(planRun(t, &loop.Options{PlanTool: litTasks, PlanNudgeEvery: -1}, 12))
	assert.Equal(t, 0, got, "nudged %d times with the reminders off", got)

	got = countNudges(planRun(t, &loop.Options{PlanNudgeEvery: 2}, 12))
	assert.Equal(t, 0, got, "nudged %d times with no plan tool to point at", got)
}

func TestPlanNudgeIsANotice(t *testing.T) {
	got := loop.PlanNudge(litTasks)
	assert.True(t, strings.HasPrefix(got, loop.NoticePrefix), "the nudge must carry the notice prefix and name the tool")
	assert.Contains(t, got, litTasks, "the nudge must carry the notice prefix and name the tool")
}

// End to end. A long run in a small window loses the model's own plan call to
// forgetting, and the run puts it back - the conversation and so the session log
// hold both the original and the reposted call.
func TestALongRunKeepsThePlanInView(t *testing.T) {
	result := planRun(t, &loop.Options{
		PlanTool:       litTasks,
		PlanNudgeEvery: -1,
		PlanMinTurns:   1000,
		ContextWindow:  2_000,
	}, 40)

	posted := 0

	for _, message := range result.Messages {
		if a := message.Activity; a != nil && a.Kind == conversation.ActivityRequest && a.Name == litTasks && strings.HasPrefix(a.ID, "plan-") {
			posted++

			assert.JSONEq(t, planArgs, a.Arguments, "the reposted plan differs from the model's")
		}
	}

	assert.NotEqual(t, 0, posted, "the plan was never posted again")
}
