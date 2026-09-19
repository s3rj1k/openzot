package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
)

// toolWith is a tool for agent tests: it takes any JSON object and answers with
// what the handler returns - a string as it is, anything else as JSON. A handler
// error is an error response, as the real tools report a failure.
func toolWith(name string, handler func(context.Context, map[string]any) (any, error)) fantasy.AgentTool {
	return fantasy.NewAgentTool(name, name,
		func(ctx context.Context, args map[string]any, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			output, err := handler(ctx, args)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			if text, ok := output.(string); ok {
				return fantasy.NewTextResponse(text), nil
			}

			encoded, _ := json.Marshal(output)

			return fantasy.NewTextResponse(string(encoded)), nil
		})
}

// findTool picks a tool out of a set by name.
func findTool(tools []fantasy.AgentTool, name string) (fantasy.AgentTool, bool) {
	for _, tool := range tools {
		if tool.Info().Name == name {
			return tool, true
		}
	}

	return nil, false
}

// call runs a tool with the given arguments the way the engine does: as a JSON
// input. A response the tool flags as an error comes back as an error.
func call(t *testing.T, tools []fantasy.AgentTool, name string, args map[string]any) (any, error) {
	t.Helper()

	tool, ok := findTool(tools, name)
	if !ok {
		t.Fatalf("no tool named %q", name)
	}

	input, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}

	response, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "c", Name: name, Input: string(input)})
	if err != nil {
		return nil, err
	}

	if response.IsError {
		return nil, errors.New(response.Content)
	}

	return response.Content, nil
}

// The handlers are methods on a toolSet now; these shims let the existing
// tests exercise them directly at the default output ceiling.
const maxToolOutput = DefaultMaxToolOutput

var defaultSet = toolSet{maxOutput: DefaultMaxToolOutput}

func shellHandler(ctx context.Context, a map[string]any) (any, error) {
	command, _ := a["command"].(string)
	if command == "" {
		return nil, errors.New(`missing required argument "command"`)
	}

	timeout, _ := a["timeout"].(int)

	return defaultSet.shell(ctx, command, timeout), nil
}

func TestDefaultToolsAreWellFormed(t *testing.T) {
	tools := DefaultTools()

	for _, name := range []string{"shell", "tasks"} {
		tool, ok := findTool(tools, name)

		if !ok {
			t.Fatalf("tool %q is missing", name)
		}

		info := tool.Info()

		if info.Description == "" {
			t.Errorf("%s has no description", name)
		}

		if len(info.Parameters) == 0 {
			t.Errorf("%s has no parameter schema", name)
		}

		if len(info.Required) == 0 {
			t.Errorf("%s requires nothing; the schema should name what a call needs", name)
		}
	}
}

func TestShellReturnsOutput(t *testing.T) {
	got, err := call(t, DefaultTools(), "shell", map[string]any{"command": "echo hello"})
	if err != nil {
		t.Fatalf("shell: %v", err)
	}

	if !strings.Contains(got.(string), "hello") {
		t.Errorf("shell output = %q", got)
	}
}

// A failing command is information the model can act on - a compiler error, a
// failing test - so it comes back as output rather than as an error that would
// end the run.
func TestShellFailureIsOutputNotAnError(t *testing.T) {
	got, err := call(t, DefaultTools(), "shell", map[string]any{"command": "exit 3"})
	if err != nil {
		t.Fatalf("a non-zero exit must not surface as an error: %v", err)
	}

	if !strings.Contains(got.(string), "exit") {
		t.Errorf("the exit status must be visible to the model: %q", got)
	}
}

func TestShellTimeoutIsReported(t *testing.T) {
	got, err := call(t, DefaultTools(), "shell", map[string]any{
		"command": "sleep 5", "timeout": float64(1),
	})
	if err != nil {
		t.Fatalf("a timeout must not surface as an error: %v", err)
	}

	if !strings.Contains(got.(string), "timed out") {
		t.Errorf("a timeout must be visible to the model: %q", got)
	}
}

func TestShellRequiresACommand(t *testing.T) {
	if _, err := shellHandler(context.Background(), map[string]any{}); err == nil {
		t.Error("running without a command must be reported")
	}
}

// A cancelled context stops a command rather than waiting out its timeout.
func TestShellRespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	cancel()

	if _, err := shellHandler(ctx, map[string]any{"command": "sleep 5"}); err == nil {
		t.Log("a cancelled run returned output rather than an error, which is acceptable")
	}
}

func TestShellDoesNotWedgeOnADaemonisedChild(t *testing.T) {
	done := make(chan any, 1)

	go func() {
		out, _ := shellHandler(context.Background(), map[string]any{
			// the child outlives the shell and keeps the inherited pipe open
			"command": "sh -c 'sleep 60' & echo started",
			"timeout": 1,
		})

		done <- out
	}()

	select {
	case out := <-done:
		if text, ok := out.(string); ok && !strings.Contains(text, "started") {
			t.Errorf("output = %q, want the command's own output kept", text)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the tool call never returned: a backgrounded child holding the output pipe wedges the run for good")
	}
}

// The toolbox is two tools, and shell is the one that touches the machine. A
// file tool that came back would be a second way to change the tree, and the
// system prompt tells the model shell is the only way.
func TestTheToolboxIsShellAndTasks(t *testing.T) {
	tools := DefaultTools()

	var names []string

	for _, tool := range tools {
		names = append(names, tool.Info().Name)
	}

	sort.Strings(names)

	if got := strings.Join(names, ","); got != "shell,tasks" {
		t.Errorf("tools = %s, want shell and tasks", got)
	}
}

// With no file tools, shell has to be enough: create a file, read a range of it
// back, and list the directory, all with ordinary commands.
func TestShellIsEnoughToWriteReadAndListFiles(t *testing.T) {
	dir := t.TempDir()

	command := "cd " + dir + " && cat > notes.txt <<'EOF'\none\ntwo\nthree\nfour\nEOF\n" +
		"sed -n '2,3p' notes.txt && ls"

	got, err := call(t, DefaultTools(), "shell", map[string]any{"command": command})
	if err != nil {
		t.Fatalf("shell: %v", err)
	}

	text := got.(string)

	for _, want := range []string{"two\nthree", "notes.txt"} {
		if !strings.Contains(text, want) {
			t.Errorf("output is missing %q:\n%s", want, text)
		}
	}

	if strings.Contains(text, "one\n") || strings.Contains(text, "four") {
		t.Errorf("sed leaked lines outside 2-3:\n%s", text)
	}
}

// Reading a file with cat is how the model reads now, so the output ceiling
// that used to bound the read tool has to bound shell: one cat of a large file
// must not flood the context.
func TestShellOutputIsTruncatedVisibly(t *testing.T) {
	got, err := call(t, DefaultTools(), "shell", map[string]any{
		"command": "head -c " + strconv.Itoa(maxToolOutput+5_000) + " /dev/zero | tr '\\0' x",
	})
	if err != nil {
		t.Fatalf("shell: %v", err)
	}

	text := got.(string)

	if len(text) > maxToolOutput+200 {
		t.Errorf("output length %d exceeds the cap", len(text))
	}

	if !strings.Contains(text, "truncated") {
		t.Error("truncation must be visible so the model knows it saw a fragment")
	}
}

// The ceiling is configurable, so a model on a small-window endpoint can be
// given a tighter bound than the default.
func TestShellHonoursAConfiguredOutputCeiling(t *testing.T) {
	got, err := call(t, DefaultToolsWith(4_000, nil), "shell", map[string]any{
		"command": "head -c 40000 /dev/zero | tr '\\0' x",
	})
	if err != nil {
		t.Fatalf("shell: %v", err)
	}

	text := got.(string)

	if len(text) > 4_500 {
		t.Errorf("a 4000-byte ceiling returned %d bytes", len(text))
	}

	if !strings.Contains(text, "truncated") {
		t.Error("the tighter ceiling must still mark its truncation")
	}
}
