package outcome

import (
	"context"
	"fmt"

	"charm.land/fantasy"
)

// The notices the loop injects when it detects a problem. They are instructions, not diagnostics, since the loop has seen something the
// model cannot see about itself. Each carries a prefix so a reader can tell a notice from the model's own output. The prefix labels
// rather than filters, and the cycle detector still works because a repeated nudge repeats along with the behavior it answers.
const NoticePrefix = "!NB:"

// CycleNotice tells the model it is repeating itself. Naming the specific behavior matters, since "you appear to be stuck"
// produces another lap and "you have called the same tool with the same arguments" produces a different approach.
func CycleNotice(detail string) string {
	if detail == "" {
		detail = "you appear to be repeating the same steps"
	}

	return fmt.Sprintf(
		"%s %s. Do not repeat that step again. Either try a materially different "+
			"approach, or stop and explain what is blocking you.",
		NoticePrefix, detail,
	)
}

// SettleNotice is the nudge to settle. The model stopped talking without
// declaring an outcome, which is not an ending.
func SettleNotice() string {
	return fmt.Sprintf(
		"%s the task is not finished until you record an outcome. Call %s when the "+
			"objective is met, or %s when it cannot be. Do not simply stop.",
		NoticePrefix, SuccessTool, FailureTool,
	)
}

// PlanNudge reminds the model that it has a plan tool. Gentle on purpose. The
// model may well be on top of it, and the aim is only that it never
// stops keeping the plan.
func PlanNudge(tool string) string {
	return fmt.Sprintf(
		"%s reminder: you have a %s tool for the plan. If a step is done, blocked or the "+
			"approach has changed, update it now; if you have no plan yet, lay one out.",
		NoticePrefix, tool,
	)
}

// TruncationNotice follows an answer the provider cut off at the token limit.
func TruncationNotice() string {
	return NoticePrefix + " your previous answer was cut off at the output limit. " +
		"Continue from exactly where it stopped, without repeating what you already wrote."
}

// terminalHandler is never what ends a run. The loop reads the call itself. It
// exists so the tool is an ordinary one to the model.
func terminalHandler[T any](context.Context, T, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return fantasy.NewTextResponse("recorded"), nil
}

// TerminalTools are the tool definitions injected into every run. They are ordinary tools to the model, the mechanism it
// already understands, and the loop intercepts them rather than dispatching to a handler.
func TerminalTools() []fantasy.AgentTool {
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
