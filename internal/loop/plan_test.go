package loop_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3rj1k/agent/internal/conversation"
	"github.com/s3rj1k/agent/internal/loop"
	"github.com/s3rj1k/agent/internal/outcome"
	"github.com/s3rj1k/agent/internal/testutils"
)

func planEngine(t *testing.T, options *loop.Options) *loop.Engine {
	t.Helper()

	options.Model = testutils.ScriptedModel(t, []string{testutils.Stop()})

	if options.ContextWindow == 0 {
		options.ContextWindow = testWindow
	}

	options.PlanTool = litTasks

	engine, err := loop.New(options)
	require.NoError(t, err)

	return engine
}

// After forgetting, a window with too little left in it gets the plan back - as
// the newest thing in it, so it is not forgotten again straight away.
func TestForgettingLeavesTooFewTurnsSoThePlanIsPosted(t *testing.T) {
	engine := planEngine(t, &loop.Options{ContextWindow: 4_000, PlanMinTurns: 5})

	messages := testutils.History(12, strings.Repeat("file content ", 60))
	forgotten := 0

	grown := fit(engine, messages, &forgotten, testutils.Starts(messages))

	require.Len(t, grown, len(messages)+2, "the conversation grew by %d messages, want the 2 of the plan", len(grown)-len(messages))

	request := engine.BuildRequest(grown, forgotten)

	var posted bool

	for _, message := range request.Messages {
		if call, ok := testutils.ToolCallOf(message); ok && call.ToolName == litTasks && call.Input == testutils.PlanArgs {
			posted = true
		}
	}

	assert.True(t, posted)
}

// planRun runs an engine that lays out a plan and then keeps working, and
// returns the conversation.
func planRun(t *testing.T, options *loop.Options, iterations int) *loop.Result {
	t.Helper()

	options.Model = testutils.ScriptedModel(t,
		[]string{testutils.Tool("p", litTasks, testutils.PlanArgs)},
		[]string{testutils.Tool("c", litEcho, "{}")},
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
		if message.Type == conversation.TypeUser && strings.Contains(message.Text, outcome.PlanNudge(litTasks)) {
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
	got := outcome.PlanNudge(litTasks)
	assert.True(t, strings.HasPrefix(got, outcome.NoticePrefix), "the nudge must carry the notice prefix and name the tool")
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

			assert.JSONEq(t, testutils.PlanArgs, a.Arguments, "the reposted plan differs from the model's")
		}
	}

	assert.NotEqual(t, 0, posted, "the plan was never posted again")
}
