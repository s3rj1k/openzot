package loop

import (
	"context"
	"fmt"

	"charm.land/fantasy"
)

// The notices the loop injects into the conversation when it detects a problem.
//
// They are written as instructions to the model rather than as diagnostics,
// because that is what they are for: the loop has noticed something the model
// cannot see about itself, and the only lever it has is the next prompt.
//
// Each is prefixed so a reader of the thread or the session log can tell a
// notice from the model's own output. The prefix is a label, not a filter: the
// cycle detector scans notices like any other message. Detection survives that
// because the nudge for a given repetition is itself identical every time it is
// injected, so an interleaved notice repeats along with the behaviour it is
// answering instead of breaking up the run of it - and the result-run detector,
// which is what fires on a tool loop, reads only tool results and never sees
// them at all.
const noticePrefix = "!NB:"

// cycleNotice tells the model it is repeating itself.
//
// Naming the specific behaviour matters. "You appear to be stuck" produces
// another lap; "you have called the same tool with the same arguments and got
// the same answer" produces a different approach.
func cycleNotice(detail string) string {
	if detail == "" {
		detail = "you appear to be repeating the same steps"
	}

	return fmt.Sprintf(
		"%s %s. Do not repeat that step again. Either try a materially different "+
			"approach, or stop and explain what is blocking you.",
		noticePrefix, detail,
	)
}

// settleNotice is the nudge to settle: the model stopped talking without
// declaring an outcome, which is not an ending.
func settleNotice() string {
	return fmt.Sprintf(
		"%s the task is not finished until you record an outcome. Call %s when the "+
			"objective is met, or %s when it cannot be. Do not simply stop.",
		noticePrefix, SuccessTool, FailureTool,
	)
}

// limitKind describes one of the run's bounded limits for the approaching-limit
// notice: what to call it, the unit its usage is counted in, and advice suited
// to that limit. Distinct per limit so a heads-up about running low on tool
// calls reads differently from one about running low on time, and the model can
// tell which constraint it is actually up against.
type limitKind struct {
	// label names the limit in the notice.
	label string

	// unit is appended to the usage count ("40 of 50 tool calls"). Empty for
	// time, whose usage already carries its own units ("1m30s of 2m0s").
	unit string

	// waste is what to spend less of as this limit nears, phrased to slot into
	// "avoid <waste>" / "stop <waste>". Empty for time, which is spent by the
	// clock rather than by an action the agent controls.
	waste string
}

var (
	iterationLimit = limitKind{
		label: "step budget",
		unit:  "steps",
		waste: "opening new lines of investigation",
	}

	toolCallLimit = limitKind{
		label: "tool-call budget",
		unit:  "tool calls",
		waste: "redundant or exploratory calls",
	}

	timeLimit = limitKind{
		label: "time budget",
	}
)

// limitCheckpointNotice tells the model it is a given fraction through one of the
// run's bounded limits. The guidance scales with how close the limit is: a nudge
// to stay aware at the halfway mark, an instruction to prioritise as it nears,
// and finish now only near the end. Telling the model to "finish now" at 50%
// would make it stop with half its budget unused, which is worse than no notice.
func limitCheckpointNotice(kind limitKind, pct int, usage string) string {
	if kind.unit != "" {
		usage += " " + kind.unit
	}

	var guidance string

	switch {
	case pct >= 90:
		// nearly out: finish now
		guidance = "You are close to the limit - stop and complete the objective now with what you already know"
		if kind.waste != "" {
			guidance += ", making no more " + kind.waste
		}
		guidance += "."

	case pct >= 70:
		// getting close: start converging
		guidance = "Start prioritising the most important remaining work"
		if kind.waste != "" {
			guidance += " and avoid " + kind.waste
		}
		guidance += "."

	default:
		// early: awareness, not urgency - plenty of budget remains
		guidance = "There is budget left; keep an eye on it and pace yourself so the task is finished before the limit."
	}

	return fmt.Sprintf(
		"%s heads-up: you are about %d%% through your %s (%s used). When the limit is "+
			"reached the run stops, even if the task is unfinished. %s",
		noticePrefix, pct, kind.label, usage, guidance,
	)
}

// truncationNotice follows an answer the provider cut off at the token limit.
func truncationNotice() string {
	return noticePrefix + " your previous answer was cut off at the output limit. " +
		"Continue from exactly where it stopped, without repeating what you already wrote."
}

// terminalTools are the tool definitions injected into every run.
//
// They are given to the model as ordinary tools because that is the mechanism it
// already understands. The loop intercepts them rather than dispatching to a
// handler.
func terminalTools() []fantasy.AgentTool {
	return []fantasy.AgentTool{
		fantasy.NewAgentTool(SuccessTool,
			"Record that the objective has been met, and end the run. Call this exactly once, when the task is genuinely complete.",
			terminalHandler[successInput]),
		fantasy.NewAgentTool(FailureTool,
			"Record that the objective cannot be met, and end the run. Call this when you are blocked and further attempts would not help.",
			terminalHandler[failureInput]),
	}
}

// successInput and failureInput are the terminal tools' schemas.
type successInput struct {
	Summary string `json:"summary" description:"What was accomplished."`
}

type failureInput struct {
	Reason string `json:"reason" description:"What is blocking completion."`
}

// terminalHandler is never what ends a run: the loop reads the call itself. It
// exists so the tool is an ordinary one to the model.
func terminalHandler[T any](context.Context, T, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.NewTextResponse("recorded"), nil
}
