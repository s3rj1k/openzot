// Package tools is the tools a run offers the model: shell, which is the only one
// that touches the machine, tasks, which keeps the run's plan followable, and
// skills, which serves instructions on request. Each is a typed fantasy tool.
package tools

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"charm.land/fantasy"
	"github.com/openzot/openzot/internal/skills"
)

// DefaultOutputPercent is the share of the context window, in percent, that a
// single tool result may take when the caller does not choose its own. See
// toolSet.truncate for why a bound exists.
const DefaultOutputPercent = 25

// ShellTool is the name of the tool that acts on the machine.
const ShellTool = "shell"

// New returns the standard tool set, with a ceiling of maxOutput bytes on a
// single tool result. Zero or negative means no ceiling.
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
//
// The ceiling is the caller's to derive from the model's context window: a
// single result that overflows the window is rejected wholesale, and the run
// cannot recover from a message it cannot even send.
func New(maxOutput int, offered []skills.Skill) []fantasy.AgentTool {
	s := toolSet{maxOutput: maxOutput}

	tools := []fantasy.AgentTool{s.shellTool(), tasksTool()}

	if len(offered) > 0 {
		tools = append(tools, s.skillsTool(offered))
	}

	return tools
}

// shellInput is what the shell tool is called with. The struct is the schema:
// fantasy generates the tool's parameters from its tags, and a field is required
// unless it is omitempty.
type shellInput struct {
	Command string `json:"command" description:"The command to run"`
	Timeout int    `json:"timeout,omitempty" description:"Timeout in seconds, default 120"`
}

func (s toolSet) shellTool() fantasy.AgentTool {
	return fantasy.NewAgentTool(ShellTool,
		"Run a shell command and return its combined output. This is your only way to act on the machine: read files (cat, head, tail, sed -n 'START,ENDp', grep -n), list directories (ls, find), create and change files, and run builds, tests and linters. Output beyond a size limit is truncated, so read large files in ranges and filter with grep rather than printing them whole.",
		func(ctx context.Context, in shellInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if in.Command == "" {
				return fantasy.NewTextErrorResponse(`missing required argument "command"`), nil
			}

			return fantasy.NewTextResponse(s.shell(ctx, in.Command, in.Timeout)), nil
		})
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
	if s.maxOutput <= 0 || len(text) <= s.maxOutput {
		return text
	}

	return text[:s.maxOutput] + fmt.Sprintf("\n\n[truncated: %d bytes total]", len(text))
}

// shell runs a command and returns its combined output. Every outcome is output,
// including a failed or timed-out command: none of them is an error the model
// could not act on.
func (s toolSet) shell(ctx context.Context, command string, timeoutSeconds int) string {
	timeout := 120 * time.Second

	if timeoutSeconds > 0 {
		timeout = time.Duration(timeoutSeconds) * time.Second
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
		return s.truncate(fmt.Sprintf("%s\n[exit: %v]", output, err))
	}

	if ctx.Err() != nil {
		return s.truncate(fmt.Sprintf("%s\n[timed out after %s]", output, timeout))
	}

	return s.truncate(string(output))
}
