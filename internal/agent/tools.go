package agent

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

// DefaultMaxToolOutput is the byte ceiling on a single tool result when the
// caller does not set its own. See toolSet.truncate for why a bound exists.
const DefaultMaxToolOutput = 100_000

// DefaultTools returns the standard tool set, with the default output ceiling
// and no skills.
//
// The set is two tools. shell is the only one that touches the machine: the
// model reads, lists, creates and changes files with ordinary commands, the way
// anyone does at a terminal, so there is one place a run's effects come from and
// one place to bound them. tasks changes nothing on disk; it exists so the work
// a run has set itself, and how far along it is, can be followed. A third, skills,
// is added when there are skills to offer.
//
// shell runs with the privileges of the process. That is the point - an agent
// that cannot touch the machine is not much use to a CLI - but it means the
// caller decides what to expose, and a caller running untrusted instructions
// should hand over a narrower set.
func DefaultTools() Tools {
	return DefaultToolsWith(DefaultMaxToolOutput, nil)
}

// DefaultToolsWith returns the standard tool set with a specific ceiling on a
// single tool result. A model served by an endpoint with a small context
// window needs a tighter bound than one with a large one - a single result
// that overflows the window is rejected wholesale, and the run cannot recover
// from a message it cannot even send. Zero or negative uses the default.
//
// skills, when there are any, adds the skills tool over them.
func DefaultToolsWith(maxOutput int, skills []Skill) Tools {
	if maxOutput <= 0 {
		maxOutput = DefaultMaxToolOutput
	}

	s := toolSet{maxOutput: maxOutput}

	tools := Tools{
		"shell": {
			Description: "Run a shell command and return its combined output. This is your only way to act on the machine: read files (cat, head, tail, sed -n 'START,ENDp', grep -n), list directories (ls, find), create and change files, and run builds, tests and linters. Output beyond a size limit is truncated, so read large files in ranges and filter with grep rather than printing them whole.",
			Parameters: FunctionParameters{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string", "description": "The command to run"},
					"timeout": map[string]any{"type": "integer", "description": "Timeout in seconds, default 120"},
				},
				"required": []string{"command"},
			},
			Handler: s.shell,
		},

		"tasks": {
			Description: "List the tasks the work needs and keep each one's status current. Call it at the start to lay the work out, then again as you go: set a task in_progress when you begin it, done when it is finished, blocked when it cannot go on. Every call carries the whole list and replaces the last, so also use it to revise the list when your approach changes. Use a task's note for what blocks it, what you found, or an assumption you made.",
			Parameters: FunctionParameters{
				"type": "object",
				"properties": map[string]any{
					"tasks": map[string]any{
						"type":        "array",
						"description": "Every task, in the order you will do them, each with its current status",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"title":  map[string]any{"type": "string", "description": "What the task is"},
								"status": map[string]any{"type": "string", "enum": []string{"pending", "in_progress", "done", "blocked"}, "description": "Where the task stands"},
								"note":   map[string]any{"type": "string", "description": "Optional: why it is blocked, what you found, or an assumption you made"},
							},
							"required": []string{"title", "status"},
						},
					},
				},
				"required": []string{"tasks"},
			},
			Handler: tasksHandler,
		},
	}

	if len(skills) > 0 {
		tools["skills"] = s.skillsTool(skills)
	}

	return tools
}

// toolSet carries the configuration the tools share - currently just the output
// ceiling. The handlers are its methods so the ceiling is captured per tool set
// rather than read from a package global, which a per-run or per-model override
// could not vary.
type toolSet struct {
	maxOutput int
}

// truncate bounds what a tool may return.
//
// An unbounded result is a context-window hazard: one cat of a large file can
// consume the whole budget and evict the conversation that explains why it was
// read - or, on an endpoint with a small window, be rejected wholesale so the
// run cannot even send it. Truncation is visible so the model knows it is
// seeing a fragment.
func (s toolSet) truncate(text string) string {
	if len(text) <= s.maxOutput {
		return text
	}

	return text[:s.maxOutput] + fmt.Sprintf("\n\n[truncated: %d bytes total]", len(text))
}

func stringArg(args map[string]any, key string) (string, error) {
	value, ok := args[key].(string)
	if !ok || value == "" {
		return "", fmt.Errorf("missing required argument %q", key)
	}

	return value, nil
}

func intArg(args map[string]any, key string) (int, bool) {
	switch value := args[key].(type) {
	case float64:
		return int(value), true
	case int:
		return value, true
	default:
		return 0, false
	}
}

func (s toolSet) shell(ctx context.Context, args map[string]any) (any, error) {
	command, err := stringArg(args, "command")
	if err != nil {
		return nil, err
	}

	timeout := 120 * time.Second

	if seconds, ok := intArg(args, "timeout"); ok && seconds > 0 {
		timeout = time.Duration(seconds) * time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)

	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)

	// Killing the shell is not enough. A command that leaves a process behind -
	// `npm start &`, anything that daemonises - hands the inherited output pipe
	// to a grandchild, and reading that pipe blocks until every holder of it is
	// gone. Without a WaitDelay the tool call simply never returns, and nothing
	// upstream can recover: the run's time budget is only checked between
	// iterations, and cancelling the run kills the shell, not the process
	// holding the pipe. WaitDelay gives up on the pipe shortly after the process
	// is killed, so a wedged command costs a timeout instead of the whole run.
	cmd.WaitDelay = 2 * time.Second

	setProcessGroup(cmd)

	output, err := cmd.CombinedOutput()

	// @note a non-zero exit is returned to the model as output rather than as an
	// error. A failing command is information - a compiler error, a failing test -
	// and the model is usually the thing best placed to act on it.
	if err != nil && ctx.Err() == nil {
		return s.truncate(fmt.Sprintf("%s\n[exit: %v]", output, err)), nil
	}

	if ctx.Err() != nil {
		return s.truncate(fmt.Sprintf("%s\n[timed out after %s]", output, timeout)), nil
	}

	return s.truncate(string(output)), nil
}
