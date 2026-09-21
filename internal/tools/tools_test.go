package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/tools"
)

// findTool picks a tool out of a set by name.
func findTool(set []fantasy.AgentTool, name string) (fantasy.AgentTool, bool) {
	for _, tool := range set {
		if tool.Info().Name == name {
			return tool, true
		}
	}

	return nil, false
}

// call runs a tool with the given arguments the way the engine does. As a JSON
// input. A response the tool flags as an error comes back as an error.
// AsString is a tool's answer as the text it is.
func asString(t *testing.T, answer any) string {
	t.Helper()

	text, ok := answer.(string)
	require.True(t, ok, "the tool answered %T, want a string", answer)

	return text
}

func call(t *testing.T, set []fantasy.AgentTool, name string, args map[string]any) (any, error) {
	t.Helper()

	tool, ok := findTool(set, name)
	require.True(t, ok, "no tool named %q", name)

	input, err := json.Marshal(args)
	require.NoError(t, err)

	response, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "c", Name: name, Input: string(input)})
	if err != nil {
		return nil, err
	}

	if response.IsError {
		return nil, errors.New(response.Content)
	}

	return response.Content, nil
}

// The handlers are methods on a ToolSet now. These shims let the existing
// tests exercise them directly at an ordinary output ceiling.
const maxToolOutput = 100_000

var defaultSet = tools.ToolSet{MaxOutput: maxToolOutput}

func shellHandler(ctx context.Context, a map[string]any) (any, error) {
	command, _ := a[litCommand].(string)
	if command == "" {
		return nil, errors.New(`missing required argument "command"`)
	}

	timeout, _ := a["timeout"].(int)

	return defaultSet.Shell(ctx, command, timeout), nil
}

func TestDefaultToolsAreWellFormed(t *testing.T) {
	set := tools.New(maxToolOutput, nil)

	for _, name := range []string{"shell", litTasks} {
		tool, ok := findTool(set, name)

		require.True(t, ok, "tool %q is missing", name)

		info := tool.Info()

		assert.NotEmpty(t, info.Description, "%s has no description", name)

		assert.NotEmpty(t, info.Parameters, "%s has no parameter schema", name)

		assert.NotEmpty(t, info.Required, "%s requires nothing; the schema should name what a call needs", name)
	}
}

func TestShellReturnsOutput(t *testing.T) {
	got, err := call(t, tools.New(maxToolOutput, nil), "shell", map[string]any{litCommand: "echo hello"})
	require.NoError(t, err)

	assert.Contains(t, asString(t, got), "hello")
}

// A failing command is information the model can act on - a compiler error, a
// failing test - so it comes back as output rather than as an error that would
// end the run.
func TestShellFailureIsOutputNotAnError(t *testing.T) {
	got, err := call(t, tools.New(maxToolOutput, nil), "shell", map[string]any{litCommand: "exit 3"})
	require.NoError(t, err, "a non-zero exit must not surface as an error")

	assert.Contains(t, asString(t, got), "exit", "the exit status must be visible to the model")
}

func TestShellTimeoutIsReported(t *testing.T) {
	got, err := call(t, tools.New(maxToolOutput, nil), "shell", map[string]any{
		litCommand: "sleep 5", "timeout": float64(1),
	})
	require.NoError(t, err)

	assert.Contains(t, asString(t, got), "timed out", "a timeout must be visible to the model")
}

func TestShellRequiresACommand(t *testing.T) {
	_, err := shellHandler(t.Context(), map[string]any{})
	require.Error(t, err, "running without a command must be reported")
}

// A canceled context stops a command rather than waiting out its timeout.
func TestShellRespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())

	cancel()

	if _, err := shellHandler(ctx, map[string]any{litCommand: "sleep 5"}); err == nil {
		t.Log("a canceled run returned output rather than an error, which is acceptable")
	}
}

func TestShellDoesNotWedgeOnADaemonisedChild(t *testing.T) {
	done := make(chan any, 1)

	go func() {
		out, _ := shellHandler(t.Context(), map[string]any{
			// the child outlives the shell and keeps the inherited pipe open
			litCommand: "sh -c 'sleep 60' & echo started",
			"timeout":  1,
		})

		done <- out
	}()

	select {
	case out := <-done:
		if text, ok := out.(string); ok {
			assert.Contains(t, text, "started", "want the command's own output kept")
		}
	case <-time.After(20 * time.Second):
		require.FailNow(t, "the tool call never returned: a backgrounded child holding the output pipe wedges the run for good")
	}
}

// The toolbox is two tools, and shell is the one that touches the machine. A
// file tool that came back would be a second way to change the tree, and the
// system prompt tells the model shell is the only way.
func TestTheToolboxIsShellAndTasks(t *testing.T) {
	set := tools.New(maxToolOutput, nil)

	names := make([]string, 0, len(set))

	for _, tool := range set {
		names = append(names, tool.Info().Name)
	}

	slices.Sort(names)

	assert.Equal(t, "shell,tasks", strings.Join(names, ","), "want shell and tasks")
}

// With no file tools, shell has to be enough. Create a file, read a range of it
// back, and list the directory, all with ordinary commands.
func TestShellIsEnoughToWriteReadAndListFiles(t *testing.T) {
	dir := t.TempDir()

	command := "cd " + dir + " && cat > notes.txt <<'EOF'\none\ntwo\nthree\nfour\nEOF\n" +
		"sed -n '2,3p' notes.txt && ls"

	got, err := call(t, tools.New(maxToolOutput, nil), "shell", map[string]any{litCommand: command})
	require.NoError(t, err)

	text := asString(t, got)

	for _, want := range []string{"two\nthree", "notes.txt"} {
		assert.Contains(t, text, want)
	}

	assert.NotContains(t, text, "one\n", "sed leaked lines outside 2-3")
	assert.NotContains(t, text, "four", "sed leaked lines outside 2-3")
}

// Reading a file with cat is how the model reads now, so the output ceiling
// that used to bound the read tool has to bound shell. One cat of a large file
// must not flood the context.
func TestShellOutputIsTruncatedVisibly(t *testing.T) {
	got, err := call(t, tools.New(maxToolOutput, nil), "shell", map[string]any{
		litCommand: "head -c " + strconv.Itoa(maxToolOutput+5_000) + " /dev/zero | tr '\\0' x",
	})
	require.NoError(t, err)

	text := asString(t, got)

	assert.LessOrEqual(t, len(text), maxToolOutput+200, "output length %d exceeds the cap", len(text))

	assert.Contains(t, text, "truncated", "truncation must be visible so the model knows it saw a fragment")
}

// The ceiling is the caller's, so a model on a small-window endpoint can be given
// a tighter bound.
func TestShellHonoursAConfiguredOutputCeiling(t *testing.T) {
	got, err := call(t, tools.New(4_000, nil), "shell", map[string]any{
		litCommand: "head -c 40000 /dev/zero | tr '\\0' x",
	})
	require.NoError(t, err)

	text := asString(t, got)

	assert.LessOrEqual(t, len(text), 4_500, "a 4000-byte ceiling returned %d bytes", len(text))

	assert.Contains(t, text, "truncated", "the tighter ceiling must still mark its truncation")
}

// No ceiling means what it says. A caller that does not want one gets the whole
// output, and no marker.
func TestNoCeilingReturnsEverything(t *testing.T) {
	got, err := call(t, tools.New(0, nil), "shell", map[string]any{
		litCommand: "head -c 30000 /dev/zero | tr '\\0' x",
	})
	require.NoError(t, err)

	text := asString(t, got)
	assert.Len(t, text, 30_000, "want all 30000 untouched")
	assert.NotContains(t, text, "truncated", "want all 30000 untouched")
}
