package loop

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/openzot/openzot/internal/conversation"
)

const planArgs = `{"tasks":[{"title":"read the code","status":"done"},{"title":"fix it","status":"in_progress"}]}`

// planCall is the model calling its plan tool and being answered.
func planCall(id, args, answer string) []conversation.Message {
	return []conversation.Message{
		activity(conversation.ActivityRequest, id, "tasks", args, nil),
		activity(conversation.ActivityResponse, id, "tasks", args, answer),
	}
}

func planEngine(t *testing.T, options Options) *Engine {
	t.Helper()

	options.Client = stub(t, []string{stop()})

	if options.ContextWindow == 0 {
		options.ContextWindow = testWindow
	}

	options.PlanTool = "tasks"

	engine, err := New(options)
	if err != nil {
		t.Fatal(err)
	}

	return engine
}

// The plan is the model's latest successful call of the plan tool, said again
// under a fresh id - and only when it has been forgotten.
func TestRepostedPlan(t *testing.T) {
	engine := planEngine(t, Options{})

	messages := append(planCall("a", `{"tasks":[]}`, "old plan"), planCall("b", planArgs, "the plan")...)
	messages = append(messages, conversation.Message{Type: conversation.TypeUser, Text: "later"})

	t.Run("a plan that fell out of the window is posted again", func(t *testing.T) {
		posted, ok := engine.repostedPlan(messages, 5)
		if !ok || len(posted) != 2 {
			t.Fatalf("posted %d messages, ok=%v, want the call and its result", len(posted), ok)
		}

		call, result := posted[0].Activity, posted[1].Activity

		if call.Kind != conversation.ActivityRequest || result.Kind != conversation.ActivityResponse || call.ID != result.ID {
			t.Errorf("not a paired call and result: %+v %+v", call, result)
		}

		if call.ID == "a" || call.ID == "b" {
			t.Errorf("the id %q is one the model already used", call.ID)
		}

		if call.Name != "tasks" || call.Arguments != planArgs || result.Result != "the plan" || posted[1].Text != "the plan" {
			t.Errorf("not the latest plan: %+v %+v", call, result)
		}
	})

	t.Run("a plan still in the window needs no help", func(t *testing.T) {
		// the plan's result is message 3: at the offset it is the oldest one held
		for _, forgotten := range []int{2, 3} {
			if _, ok := engine.repostedPlan(messages, forgotten); ok {
				t.Errorf("the plan was posted while its result is still in the window (offset %d)", forgotten)
			}
		}

		if _, ok := engine.repostedPlan(messages, 4); !ok {
			t.Error("the plan was not posted with its result one message out of the window")
		}
	})

	t.Run("no plan, nothing to post", func(t *testing.T) {
		if _, ok := engine.repostedPlan([]conversation.Message{{Type: conversation.TypeUser, Text: "hi"}}, 1); ok {
			t.Error("a plan was invented")
		}
	})

	t.Run("a refused call is not the plan", func(t *testing.T) {
		refused := append([]conversation.Message(nil), messages[:4]...)
		refused = append(refused,
			activity(conversation.ActivityRequest, "c", "tasks", `{"tasks":[]}`, nil),
			conversation.Message{Type: conversation.TypeActivity, Activity: &conversation.Activity{Kind: conversation.ActivityResponse, ID: "c", Name: "tasks", Arguments: `{"tasks":[]}`, Failure: "tasks needs at least one task"}},
			conversation.Message{Type: conversation.TypeUser, Text: "later"},
		)

		posted, ok := engine.repostedPlan(refused, 6)
		if !ok || posted[0].Activity.Arguments != planArgs {
			t.Errorf("the plan should be the last one that worked, got %+v (ok=%v)", posted, ok)
		}
	})

	t.Run("another tool is not the plan", func(t *testing.T) {
		other := []conversation.Message{activity(conversation.ActivityResponse, "x", "shell", "{}", "out"), {Type: conversation.TypeUser, Text: "later"}}

		if _, ok := engine.repostedPlan(other, 1); ok {
			t.Error("a shell result was taken for the plan")
		}
	})

	t.Run("without a plan tool there is no plan", func(t *testing.T) {
		bare, err := New(Options{Client: stub(t, []string{stop()}), ContextWindow: testWindow})
		if err != nil {
			t.Fatal(err)
		}

		if _, ok := bare.repostedPlan(messages, 5); ok {
			t.Error("a plan was posted with no plan tool")
		}
	})
}

// history is a plan, then n turns of tool work each costing about cost tokens.
func history(n int, filler string) []conversation.Message {
	messages := []conversation.Message{{Type: conversation.TypeUser, Text: "kickoff"}}
	messages = append(messages, planCall("plan", planArgs, "the plan")...)

	for i := range n {
		id := fmt.Sprintf("c%d", i)

		messages = append(messages,
			activity(conversation.ActivityRequest, id, "read", `{"path":"x"}`, nil),
			activity(conversation.ActivityResponse, id, "read", `{"path":"x"}`, filler),
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
	engine := planEngine(t, Options{ContextWindow: 4_000, PlanMinTurns: 5})

	messages := history(12, strings.Repeat("file content ", 60))
	forgotten := 0

	grown := engine.fitToWindow(messages, &forgotten, starts(messages), nil, func(Event) {})

	if len(grown) != len(messages)+2 {
		t.Fatalf("the conversation grew by %d messages, want the 2 of the plan", len(grown)-len(messages))
	}

	request := engine.buildRequest(grown, forgotten)

	var posted bool

	for _, message := range request.messages {
		if call, ok := toolCallOf(message); ok && call.ToolName == "tasks" && call.Input == planArgs {
			posted = true
		}
	}

	if !posted {
		t.Error("the plan is not in the request")
	}
}

// A turn is whole only if none of it was forgotten, and the one being asked for
// has not happened yet.
func TestTurnsHeld(t *testing.T) {
	starts := []int{0, 4, 8, 12, 16}

	for forgotten, want := range map[int]int{0: 4, 4: 3, 5: 2, 8: 2, 13: 0, 16: 0, 40: 0} {
		if got := turnsHeld(starts, forgotten); got != want {
			t.Errorf("offset %d holds %d turns, want %d", forgotten, got, want)
		}
	}
}

// The plan goes back when the window holds fewer turns than plan_min_turns, and
// not when it holds exactly that many.
func TestThePlanGoesBackOnlyBelowTheMinimumTurns(t *testing.T) {
	messages := history(12, strings.Repeat("file content ", 60))
	marks := starts(messages)

	// what the window holds when the decision is made: after forgetting, before
	// the plan is put back
	probe := planEngine(t, Options{ContextWindow: 4_000})
	forgotten := 0
	probe.forgetOldest(messages, &forgotten, nil, func(Event) {})

	held := turnsHeld(marks, forgotten)
	if held == 0 {
		t.Fatal("test setup: no turns held")
	}

	for min, wantPosted := range map[int]bool{held - 1: false, held: false, held + 1: true} {
		engine := planEngine(t, Options{ContextWindow: 4_000, PlanMinTurns: min})
		offset := 0

		grown := engine.fitToWindow(messages, &offset, marks, nil, func(Event) {})

		if posted := len(grown) > len(messages); posted != wantPosted {
			t.Errorf("with %d turns held and a minimum of %d, posted = %v, want %v", held, min, posted, wantPosted)
		}
	}
}

func TestThePlanIsLeftAloneWhenTheWindowStillHoldsEnoughTurns(t *testing.T) {
	engine := planEngine(t, Options{ContextWindow: 4_000, PlanMinTurns: 2})

	messages := history(12, strings.Repeat("file content ", 60))
	forgotten := 0

	grown := engine.fitToWindow(messages, &forgotten, starts(messages), nil, func(Event) {})

	if forgotten == 0 {
		t.Fatal("test setup: nothing was forgotten")
	}

	if len(grown) != len(messages) {
		t.Errorf("the plan was posted with plenty of turns left (%d messages added)", len(grown)-len(messages))
	}
}

func TestNothingIsPostedWhileNothingIsForgotten(t *testing.T) {
	engine := planEngine(t, Options{PlanMinTurns: 100})

	messages := history(3, "short")
	forgotten := 0

	if grown := engine.fitToWindow(messages, &forgotten, starts(messages), nil, func(Event) {}); len(grown) != len(messages) {
		t.Error("the plan was posted though the whole conversation fits")
	}
}

func TestThePlanIsNotPostedTwice(t *testing.T) {
	engine := planEngine(t, Options{ContextWindow: 4_000, PlanMinTurns: 100})

	messages := history(12, strings.Repeat("file content ", 60))
	forgotten := 0

	messages = engine.fitToWindow(messages, &forgotten, starts(messages), nil, func(Event) {})
	size := len(messages)

	// the next request forgets again, but the posted plan is well inside the window
	messages = append(messages,
		activity(conversation.ActivityRequest, "n", "read", `{"path":"y"}`, nil),
		activity(conversation.ActivityResponse, "n", "read", `{"path":"y"}`, strings.Repeat("file content ", 60)),
	)

	messages = engine.fitToWindow(messages, &forgotten, append(starts(messages[:size]), size, len(messages)), nil, func(Event) {})

	if len(messages) != size+2 {
		t.Errorf("the plan was posted again while the last copy is in the window (%d extra messages)", len(messages)-size-2)
	}
}

// planRun runs an engine that lays out a plan and then keeps working, and
// returns the conversation.
func planRun(t *testing.T, options Options, iterations int) Result {
	t.Helper()

	options.Client = stub(t,
		[]string{tool("p", "tasks", planArgs)},
		[]string{tool("c", "echo", "{}")},
	)
	options.Tools = append(echoTool(new(int)), namedTool("tasks", func(context.Context) (any, error) { return "tasks: 0/2 done", nil }))
	options.MaxIterations = iterations
	options.MaxCycles = 100000
	options.RetryBackoff = -1

	if options.ContextWindow == 0 {
		options.ContextWindow = testWindow
	}

	engine, err := New(options)
	if err != nil {
		t.Fatal(err)
	}

	return engine.Run(t.Context(), nil)
}

func countNudges(result Result) int {
	nudges := 0

	for _, message := range result.Messages {
		if message.Type == conversation.TypeUser && strings.Contains(message.Text, planNudge("tasks")) {
			nudges++
		}
	}

	return nudges
}

func TestThePlanToolIsRememberedEveryNthIteration(t *testing.T) {
	// iterations 3, 6 and 9 of ten
	if got := countNudges(planRun(t, Options{PlanTool: "tasks", PlanNudgeEvery: 3}, 10)); got != 3 {
		t.Errorf("nudged %d times, want 3", got)
	}

	// the default is every fifth
	if got := countNudges(planRun(t, Options{PlanTool: "tasks"}, 11)); got != 2 {
		t.Errorf("nudged %d times with the default, want 2 (iterations 5 and 10)", got)
	}
}

func TestPlanRemindersCanBeSwitchedOffAndNeedAPlanTool(t *testing.T) {
	if got := countNudges(planRun(t, Options{PlanTool: "tasks", PlanNudgeEvery: -1}, 12)); got != 0 {
		t.Errorf("nudged %d times with the reminders off", got)
	}

	if got := countNudges(planRun(t, Options{PlanNudgeEvery: 2}, 12)); got != 0 {
		t.Errorf("nudged %d times with no plan tool to point at", got)
	}
}

func TestPlanNudgeIsANotice(t *testing.T) {
	if got := planNudge("tasks"); !strings.HasPrefix(got, noticePrefix) || !strings.Contains(got, "tasks") {
		t.Errorf("the nudge must carry the notice prefix and name the tool: %q", got)
	}
}

// End to end: a long run in a small window loses the model's own plan call to
// forgetting, and the run puts it back - the conversation and so the session log
// hold both the original and the reposted call.
func TestALongRunKeepsThePlanInView(t *testing.T) {
	result := planRun(t, Options{
		PlanTool:       "tasks",
		PlanNudgeEvery: -1,
		PlanMinTurns:   1000,
		ContextWindow:  2_000,
	}, 40)

	posted := 0

	for _, message := range result.Messages {
		if a := message.Activity; a != nil && a.Kind == conversation.ActivityRequest && a.Name == "tasks" && strings.HasPrefix(a.ID, "plan-") {
			posted++

			if a.Arguments != planArgs {
				t.Errorf("the reposted plan differs from the model's: %q", a.Arguments)
			}
		}
	}

	if posted == 0 {
		t.Error("the plan was never posted again")
	}
}
