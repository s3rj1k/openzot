// Package window holds a conversation under a model's context window. Nothing is summarized or lost, since the session log
// keeps every message. From the soft mark the oldest message is forgotten on each request, at the hard mark as many as it
// takes, and a plan that was forgotten is posted again. Token costs are estimates, priced conservatively by UTF-8 bytes.
package window

import (
	"encoding/json"
	"fmt"
	"slices"

	"charm.land/fantasy"

	"github.com/s3rj1k/agent/internal/conversation"
	"github.com/s3rj1k/agent/internal/failure"
)

// The defaults of the window rules, applied where a caller leaves one at zero.
const (
	// NarrowFloor is the share of the configured window a rejection can narrow
	// the effective window down to, as a divisor. A provider that keeps saying
	// "too long" is wrong about its own ceiling only so far.
	NarrowFloor = 4

	// DefaultSoft is the share of the context window, in percent, at which
	// the oldest message starts being forgotten on every request.
	DefaultSoft = 50

	// DefaultHard is the share of the window a request is never allowed to
	// reach. Past it, as many of the oldest messages are forgotten as it takes.
	DefaultHard = 90

	// DefaultPlanNudgeEvery is how many iterations pass between reminders that
	// the plan tool exists and should be kept current.
	DefaultPlanNudgeEvery = 5

	// DefaultPlanMinTurns is how few turns may be left in the window, after
	// forgetting, before the plan is posted again. A window that holds fewer than
	// this has probably lost the model's last word on it.
	DefaultPlanMinTurns = 5
)

// Options configures a Fitter. A zero value takes its default.
type Options struct {
	// Window is the model's context window, in tokens. Required.
	Window int

	// Soft and Hard are the percent of the window where forgetting starts, and where as many are forgotten as it takes.
	Soft int
	Hard int

	// PlanMinTurns is how few turns may be left after forgetting before the plan is posted again.
	PlanMinTurns int

	// PlanTool is the name of the tool the model keeps its plan with. Empty means there is none to repost.
	PlanTool string

	// Instructions is the system prompt, which is sent with every request and so fills part of the window.
	Instructions string
}

// Fitter holds a conversation under a context window. It tracks the window in force, which is the configured one lowered
// when a provider rejects a request, and decides what a request may carry. It reports what it did as notices for the
// caller to show, and emits nothing itself.
type Fitter struct {
	// Size is the window requests are held under. The configured one, lowered when a provider rejects a request and
	// states its own ceiling.
	Size int

	// Soft and Hard are the percent of Size where forgetting starts and where it must reach.
	Soft int
	Hard int

	configured   int
	planTurns    int
	planTool     string
	instructions string

	// toolTokens caches the cost of the tool schemas, which are the same on every request of a run and would otherwise
	// be re-counted each round.
	toolTokens int
}

// NewFitter builds a Fitter, filling in the defaults.
func NewFitter(options Options) *Fitter {
	pick := func(value, fallback int) int {
		if value > 0 {
			return value
		}

		return fallback
	}

	return &Fitter{
		Size:         options.Window,
		Soft:         pick(options.Soft, DefaultSoft),
		Hard:         pick(options.Hard, DefaultHard),
		configured:   options.Window,
		planTurns:    pick(options.PlanMinTurns, DefaultPlanMinTurns),
		planTool:     options.PlanTool,
		instructions: options.Instructions,
	}
}

// toolSchemaTokens is what the tool definitions cost on the wire. They go with every request and can run to thousands of
// tokens, so leaving them out of the budget is how a request that seems to fit gets rejected.
func (f *Fitter) toolSchemaTokens(tools []fantasy.Tool) int {
	if f.toolTokens > 0 || len(tools) == 0 {
		return f.toolTokens
	}

	encoded, err := json.Marshal(tools)
	if err != nil {
		return 0
	}

	f.toolTokens = EstimateTokens(string(encoded))

	return f.toolTokens
}

// Narrow lowers the window requests are held under after a provider rejected one as too long, and reports whether it
// could. A provider that states its own ceiling is believed, and one that only says "too long" costs a quarter of the
// window, down to a floor. It returns false once narrowing has stopped helping. The notice says what happened.
func (f *Fitter) Narrow(limit failure.ContextLimit) (notice string, ok bool) {
	if limit.SuggestedLimit > 0 && limit.SuggestedLimit < f.Size {
		f.Size = limit.SuggestedLimit

		return fmt.Sprintf(
			"provider reported a %d token window; retrying under %d",
			limit.MaxTokens, limit.SuggestedLimit), true
	}

	narrowed := f.Size * 3 / 4

	if narrowed < f.configured/NarrowFloor {
		return "", false
	}

	f.Size = narrowed

	return fmt.Sprintf("provider rejected the request as too long; retrying under %d tokens", narrowed), true
}

// TurnsHeld is how many whole turns the window still holds. The turn about to be
// asked for is the last of turnStarts and has not happened yet, so it is not one.
func TurnsHeld(turnStarts []int, forgotten int) int {
	turns := 0

	for _, start := range turnStarts[:len(turnStarts)-1] {
		if start >= forgotten {
			turns++
		}
	}

	return turns
}

// ForgetOldest moves the offset forward as far as the window calls for. It returns a notice when it moved, and
// otherwise the empty string.
func (f *Fitter) ForgetOldest(messages []conversation.Message, forgotten *int, tools []fantasy.Tool) string {
	// the system prompt and the tool schemas are sent on every request and are
	// part of what fills the window
	used := EstimateTokens(f.instructions) + f.toolSchemaTokens(tools)

	for _, message := range messages[*forgotten:] {
		used += Cost(message)
	}

	next := Forget(messages, *forgotten, used, f.Size, f.Soft, f.Hard, Cost)
	if next == *forgotten {
		return ""
	}

	notice := fmt.Sprintf("forgot %d older messages to stay within the context window", next-*forgotten)

	*forgotten = next

	return notice
}

// RepostedPlan is the model's latest plan, its last successful plan-tool call, as a fresh call and result to append. It reports
// false when there is no plan or the plan is still in the window and needs no help.
func (f *Fitter) RepostedPlan(messages []conversation.Message, forgotten int) ([]conversation.Message, bool) {
	if f.planTool == "" {
		return nil, false
	}

	for index, message := range slices.Backward(messages) {
		activity := message.Activity

		if activity == nil || activity.Kind != conversation.ActivityResponse || activity.Name != f.planTool || activity.Failure != "" {
			continue
		}

		if index >= forgotten {
			return nil, false
		}

		id := fmt.Sprintf("plan-%d", len(messages))

		call := conversation.Activity{Kind: conversation.ActivityRequest, ID: id, Name: activity.Name, Arguments: activity.Arguments}
		answer := conversation.Activity{Kind: conversation.ActivityResponse, ID: id, Name: activity.Name, Arguments: activity.Arguments, Result: activity.Result}

		return []conversation.Message{
			{Type: conversation.TypeActivity, Activity: &call},
			{Type: conversation.TypeActivity, Text: answer.ResultText(), Activity: &answer},
		}, true
	}

	return nil, false
}

// Fit forgets the oldest messages as the window fills and puts the plan back in front of the model when forgetting left
// it too little to go on. It returns the conversation, grown by the plan when posted, and the notices in the order they
// arose. The forgotten offset only moves forward.
func (f *Fitter) Fit(messages []conversation.Message, forgotten *int, turnStarts []int, tools []fantasy.Tool) ([]conversation.Message, []string) {
	notice := f.ForgetOldest(messages, forgotten, tools)
	if notice == "" {
		return messages, nil
	}

	notices := []string{notice}

	turns := TurnsHeld(turnStarts, *forgotten)

	if turns >= f.planTurns {
		return messages, notices
	}

	posted, ok := f.RepostedPlan(messages, *forgotten)
	if !ok {
		return messages, notices
	}

	notices = append(notices, fmt.Sprintf("only %d turns are left in the context window; posting the plan again", turns))

	messages = append(messages, posted...)

	// the plan costs something too
	if notice := f.ForgetOldest(messages, forgotten, tools); notice != "" {
		notices = append(notices, notice)
	}

	return messages, notices
}
