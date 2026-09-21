package window_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/conversation"
	"github.com/openzot/openzot/internal/failure"
	"github.com/openzot/openzot/internal/testutils"
	"github.com/openzot/openzot/internal/window"
)

// testWindow is large enough that no test forgets by accident.
const testWindow = 1_000_000

// planFitter is a fitter that keeps its plan with the tasks tool, in a large window unless told otherwise.
func planFitter(options window.Options) *window.Fitter {
	if options.Window == 0 {
		options.Window = testWindow
	}

	options.PlanTool = litTasks

	return window.NewFitter(options)
}

// fit runs the fitter and returns the conversation it leaves, dropping the notices.
func fit(fitter *window.Fitter, messages []conversation.Message, forgotten *int, turnStarts []int) []conversation.Message {
	grown, _ := fitter.Fit(messages, forgotten, turnStarts, nil)

	return grown
}

// The plan is the model's latest successful call of the plan tool, said again
// under a fresh id - and only when it has been forgotten.
func TestRepostedPlan(t *testing.T) {
	fitter := planFitter(window.Options{})

	messages := append(testutils.PlanCall("a", `{"tasks":[]}`, "old plan"), testutils.PlanCall("b", testutils.PlanArgs, "the plan")...)
	messages = append(messages, conversation.Message{Type: conversation.TypeUser, Text: litLater})

	t.Run("a plan that fell out of the window is posted again", func(t *testing.T) {
		posted, ok := fitter.RepostedPlan(messages, 5)
		require.True(t, ok, "want the call and its result")
		require.Len(t, posted, 2, "want the call and its result")

		call, result := posted[0].Activity, posted[1].Activity

		assert.Equal(t, conversation.ActivityRequest, call.Kind)
		assert.Equal(t, conversation.ActivityResponse, result.Kind)
		assert.Equal(t, result.ID, call.ID)

		assert.NotEqual(t, "a", call.ID, "the id %q is one the model already used", call.ID)
		assert.NotEqual(t, "b", call.ID, "the id %q is one the model already used", call.ID)

		assert.Equal(t, litTasks, call.Name)
		assert.JSONEq(t, testutils.PlanArgs, call.Arguments)
		assert.Equal(t, "the plan", result.Result)
		assert.Equal(t, "the plan", posted[1].Text)
	})

	t.Run("a plan still in the window needs no help", func(t *testing.T) {
		// the plan's result is message 3. At the offset it is the oldest one held
		for _, forgotten := range []int{2, 3} {
			_, ok := fitter.RepostedPlan(messages, forgotten)
			assert.False(t, ok, "the plan was posted while its result is still in the window (offset %d)", forgotten)
		}

		_, ok := fitter.RepostedPlan(messages, 4)
		assert.True(t, ok, "the plan was not posted with its result one message out of the window")
	})

	t.Run("no plan, nothing to post", func(t *testing.T) {
		_, ok := fitter.RepostedPlan([]conversation.Message{{Type: conversation.TypeUser, Text: "hi"}}, 1)
		assert.False(t, ok)
	})

	t.Run("a refused call is not the plan", func(t *testing.T) {
		refused := append([]conversation.Message(nil), messages[:4]...)
		refused = append(refused,
			testutils.Activity(conversation.ActivityRequest, "c", litTasks, `{"tasks":[]}`, nil),
			conversation.Message{Type: conversation.TypeActivity, Activity: &conversation.Activity{Kind: conversation.ActivityResponse, ID: "c", Name: litTasks, Arguments: `{"tasks":[]}`, Failure: "tasks needs at least one task"}},
			conversation.Message{Type: conversation.TypeUser, Text: litLater},
		)

		posted, ok := fitter.RepostedPlan(refused, 6)
		assert.True(t, ok, "the plan should be the last one that worked, got %+v (ok=%v)", posted, ok)
		assert.JSONEq(t, testutils.PlanArgs, posted[0].Activity.Arguments, "want the last plan that worked")
	})

	t.Run("another tool is not the plan", func(t *testing.T) {
		other := []conversation.Message{testutils.Activity(conversation.ActivityResponse, "x", "shell", "{}", "out"), {Type: conversation.TypeUser, Text: litLater}}

		_, ok := fitter.RepostedPlan(other, 1)
		assert.False(t, ok, "a shell result was taken for the plan")
	})

	t.Run("without a plan tool there is no plan", func(t *testing.T) {
		bare := window.NewFitter(window.Options{Window: testWindow})

		_, ok := bare.RepostedPlan(messages, 5)
		assert.False(t, ok, "a plan was posted with no plan tool")
	})
}

// A turn is whole only if none of it was forgotten, and the one being asked for
// has not happened yet.
func TestTurnsHeld(t *testing.T) {
	starts := []int{0, 4, 8, 12, 16}

	for forgotten, want := range map[int]int{0: 4, 4: 3, 5: 2, 8: 2, 13: 0, 16: 0, 40: 0} {
		got := window.TurnsHeld(starts, forgotten)
		assert.Equal(t, want, got, "offset %d holds %d turns, want %d", forgotten, got, want)
	}
}

// The plan goes back when the window holds fewer turns than plan_min_turns, and
// not when it holds exactly that many.
func TestThePlanGoesBackOnlyBelowTheMinimumTurns(t *testing.T) {
	messages := testutils.History(12, strings.Repeat("file content ", 60))
	marks := testutils.Starts(messages)

	// what the window holds when the decision is made. After forgetting, before
	// the plan is put back
	probe := planFitter(window.Options{Window: 4_000})
	forgotten := 0
	probe.ForgetOldest(messages, &forgotten, nil)

	held := window.TurnsHeld(marks, forgotten)
	require.NotEqual(t, 0, held, "test setup: no turns held")

	for min, wantPosted := range map[int]bool{held - 1: false, held: false, held + 1: true} {
		fitter := planFitter(window.Options{Window: 4_000, PlanMinTurns: min})
		offset := 0

		grown := fit(fitter, messages, &offset, marks)

		posted := len(grown) > len(messages)
		assert.Equal(t, wantPosted, posted, "with %d turns held and a minimum of %d, posted = %v, want %v", held, min, posted, wantPosted)
	}
}

func TestThePlanIsLeftAloneWhenTheWindowStillHoldsEnoughTurns(t *testing.T) {
	fitter := planFitter(window.Options{Window: 4_000, PlanMinTurns: 2})

	messages := testutils.History(12, strings.Repeat("file content ", 60))
	forgotten := 0

	grown := fit(fitter, messages, &forgotten, testutils.Starts(messages))

	require.NotEqual(t, 0, forgotten, "test setup: nothing was forgotten")

	assert.Len(t, grown, len(messages), "the plan was posted with plenty of turns left (%d messages added)", len(grown)-len(messages))
}

func TestNothingIsPostedWhileNothingIsForgotten(t *testing.T) {
	fitter := planFitter(window.Options{PlanMinTurns: 100})

	messages := testutils.History(3, "short")
	forgotten := 0

	assert.Len(t, fit(fitter, messages, &forgotten, testutils.Starts(messages)), len(messages), "the plan was posted though the whole conversation fits")
}

func TestThePlanIsNotPostedTwice(t *testing.T) {
	fitter := planFitter(window.Options{Window: 4_000, PlanMinTurns: 100})

	messages := testutils.History(12, strings.Repeat("file content ", 60))
	forgotten := 0

	messages = fit(fitter, messages, &forgotten, testutils.Starts(messages))
	size := len(messages)

	// the next request forgets again, but the posted plan is well inside the window
	messages = append(messages,
		testutils.Activity(conversation.ActivityRequest, "n", litRead, `{"path":"y"}`, nil),
		testutils.Activity(conversation.ActivityResponse, "n", litRead, `{"path":"y"}`, strings.Repeat("file content ", 60)),
	)

	messages = fit(fitter, messages, &forgotten, append(testutils.Starts(messages[:size]), size, len(messages)))

	assert.Len(t, messages, size+2, "the plan was posted again while the last copy is in the window (%d extra messages)", len(messages)-size-2)
}

// A conversation growing round by round. Nothing is forgotten until the soft
// mark, then the request loses one message per round, and it never reaches the
// hard mark. The conversation handed in is never touched.
func TestARequestNeverReachesTheHardMark(t *testing.T) {
	const size = 20_000

	fitter := window.NewFitter(window.Options{Window: size})

	var (
		messages  = []conversation.Message{{Type: conversation.TypeUser, Text: "the kickoff"}}
		forgotten int
		firstLoss = -1
		hard      = size * window.DefaultHard / 100
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

		messages = fit(fitter, messages, &forgotten, []int{len(messages)})

		require.GreaterOrEqual(t, forgotten, before, "round %d: the offset moved back from %d to %d", round, before, forgotten)

		if forgotten > before && firstLoss < 0 {
			firstLoss = round
		}

		// what the request carries is what is left after the offset
		used := window.EstimateTokens("")

		for _, message := range messages[forgotten:] {
			used += window.Cost(message)
		}

		require.Less(t, used, hard, "round %d: the request costs %d, at or past the hard mark %d", round, used, hard)
	}

	require.GreaterOrEqual(t, firstLoss, 0, "nothing was ever forgotten")

	assert.Len(t, messages, 1+2*120)
}

// A provider that keeps saying "too long" is wrong about its own ceiling only so
// far. Once the window is down to a fraction of the configured one there is
// nothing left to try, and retrying would send the same request again.
func TestNarrowingStopsAtTheFloor(t *testing.T) {
	fitter := window.NewFitter(window.Options{Window: testWindow})

	floor := testWindow / window.NarrowFloor

	fitter.Size = floor

	// no stated window, so the only move is stepping the window down
	_, ok := fitter.Narrow(failure.ContextLimit{})
	assert.False(t, ok, "the window narrowed to %d, below the %d floor", fitter.Size, floor)

	assert.Equal(t, floor, fitter.Size)
}

// A rejection without a stated window steps the budget down by a quarter.
func TestNarrowingWithoutAStatedWindowStepsDown(t *testing.T) {
	fitter := window.NewFitter(window.Options{Window: 40_000})

	notice, ok := fitter.Narrow(failure.ContextLimit{})
	require.True(t, ok)

	assert.Equal(t, 30_000, fitter.Size)
	assert.Contains(t, notice, "30000", "the notice should say what the window is now")
}

// A provider that states its own ceiling is believed, at the share of it a retry aims for.
func TestNarrowingAdoptsAStatedCeiling(t *testing.T) {
	fitter := window.NewFitter(window.Options{Window: 40_000})

	notice, ok := fitter.Narrow(failure.ContextLimit{MaxTokens: 8192, SuggestedLimit: 6963})
	require.True(t, ok)

	assert.Equal(t, 6963, fitter.Size)
	assert.Contains(t, notice, "8192")
}

// The soft and hard marks default when a caller leaves them at zero, and the window is the configured one to begin with.
func TestAFitterTakesItsDefaults(t *testing.T) {
	fitter := window.NewFitter(window.Options{Window: 32_000})

	assert.Equal(t, 32_000, fitter.Size)
	assert.Equal(t, window.DefaultSoft, fitter.Soft)
	assert.Equal(t, window.DefaultHard, fitter.Hard)
}
