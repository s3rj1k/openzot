package tools

import (
	"context"
	"encoding/json"

	"charm.land/fantasy"

	"github.com/openzot/openzot/internal/plan"
)

// tasksInput is what the tasks tool is called with; the struct is the schema.
type tasksInput struct {
	Tasks []taskInput `json:"tasks" description:"Every task, in the order you will do them, each with its current status"`
}

type taskInput struct {
	Title  string `json:"title" description:"What the task is"`
	Status string `json:"status" enum:"pending,in_progress,done,blocked" description:"Where the task stands"`
	Note   string `json:"note,omitempty" description:"Optional: why it is blocked, what you found, or an assumption you made"`
}

// tasksTool records the task list.
//
// Tasks is a reflective tool: it changes nothing on disk. Its value is that the
// model has to state what the work needs and where each part stands, in a
// structured form, which both organises its own reasoning and makes the run
// followable in the viewer and the session log. Every call carries the whole
// list and replaces the last, so there is nothing to merge and no state here:
// the latest call is the truth.
//
// The call is still read through ParseTasks, the one place the schema is
// interpreted, so what the tool accepts and what the viewer shows cannot drift.
func tasksTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(plan.Tool,
		"List the tasks the work needs and keep each one's status current. Call it at the start to lay the work out, then again as you go: set a task in_progress when you begin it, done when it is finished, blocked when it cannot go on. Every call carries the whole list and replaces the last, so also use it to revise the list when your approach changes. Use a task's note for what blocks it, what you found, or an assumption you made.",
		func(_ context.Context, _ tasksInput, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			var args map[string]any

			if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			tasks, err := plan.ParseTasks(args)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			return fantasy.NewTextResponse(plan.FormatTasks(tasks)), nil
		})
}
