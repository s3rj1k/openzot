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
// injected, so an interleaved notice repeats along with the behavior it is
// answering instead of breaking up the run of it - and the result-run detector,
// which is what fires on a tool loop, reads only tool results and never sees
// them at all.
const noticePrefix = "!NB:"

// cycleNotice tells the model it is repeating itself.
//
// Naming the specific behavior matters. "You appear to be stuck" produces
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

// planNudge reminds the model that it has a plan tool. Gentle on purpose: the
// model may well be on top of it, and the aim is only that it never quietly
// stops keeping the plan.
func planNudge(tool string) string {
	return fmt.Sprintf(
		"%s reminder: you have a %s tool for the plan. If a step is done, blocked or the "+
			"approach has changed, update it now; if you have no plan yet, lay one out.",
		noticePrefix, tool,
	)
}

// truncationNotice follows an answer the provider cut off at the token limit.
func truncationNotice() string {
	return noticePrefix + " your previous answer was cut off at the output limit. " +
		"Continue from exactly where it stopped, without repeating what you already wrote."
}

// terminalHandler is never what ends a run: the loop reads the call itself. It
// exists so the tool is an ordinary one to the model.
func terminalHandler[T any](context.Context, T, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.NewTextResponse("recorded"), nil
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
