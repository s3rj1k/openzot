package tools

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/plan"
)

// taskCall builds the arguments of a call to the tasks tool the way the model
// sends them. JSON-decoded, so lists are []any and objects are map[string]any.
func taskCall(tasks ...map[string]any) map[string]any {
	list := make([]any, 0, len(tasks))

	for _, task := range tasks {
		list = append(list, task)
	}

	return map[string]any{litTasks: list}
}

func task(title, status string) map[string]any {
	return map[string]any{litTitle: title, litStatus: status}
}

// The tool answers with the list, so the model reads its own state back on every
// call - the latest result is the one place the whole list is always in view.
func TestTheTasksToolAnswersWithTheChecklist(t *testing.T) {
	got, err := call(t, New(maxToolOutput, nil), litTasks, taskCall(
		task("read the parser", "done"),
		map[string]any{litTitle: "fix the lexer", litStatus: "in_progress", "note": "hit in TestLex"},
		task("add a test", "pending"),
		map[string]any{litTitle: litDeploy, litStatus: "blocked", "note": "needs credentials"},
	))
	require.NoError(t, err)

	want := "tasks: 1/4 done\n" +
		"[x] read the parser\n" +
		"[>] fix the lexer - hit in TestLex\n" +
		"[ ] add a test\n" +
		"[!] deploy - needs credentials"

	assert.Equal(t, want, got)
}

// Every call carries the whole list and replaces the last. There is no state in
// the tool, so a second call is never merged into the first. A task left out of
// it is a task that is no longer wanted.
func TestTasksReplaceRatherThanMerge(t *testing.T) {
	tools := New(maxToolOutput, nil)

	_, err := call(t, tools, litTasks, taskCall(task("first", "pending"), task("second", "pending")))
	require.NoError(t, err)

	got, err := call(t, tools, litTasks, taskCall(task("third", "in_progress")))
	require.NoError(t, err)

	text := asString(t, got)

	assert.Contains(t, text, "0/1 done", "the second list should stand alone")
	assert.Contains(t, text, "third", "the second list should stand alone")

	assert.NotContains(t, text, "first", "the second call kept tasks from the first")
	assert.NotContains(t, text, "second", "the second call kept tasks from the first")
}

func TestTheTasksToolRefusesAMalformedList(t *testing.T) {
	_, err := call(t, New(maxToolOutput, nil), litTasks, map[string]any{litTasks: []any{}})
	require.Error(t, err, "an empty list must be refused")

	_, err = call(t, New(maxToolOutput, nil), litTasks, taskCall(task("a", "finished")))
	require.Error(t, err, "an unknown status must be refused")
}

// schemaAt follows a path through a tool's parameter schema.
func schemaAt(t *testing.T, schema map[string]any, path ...string) any {
	t.Helper()

	var node any = schema

	for _, key := range path {
		fields, ok := node.(map[string]any)
		require.True(t, ok, "the schema has no object at %q on the way to %v", key, path)

		node = fields[key]
	}

	return node
}

// What the model is told the tool accepts has to be what the tool accepts. The
// schema names the statuses, so a status the schema offers but the parser
// rejects would be a trap.
func TestTheSchemaOffersOnlyStatusesTheParserAccepts(t *testing.T) {
	tasks, _ := findTool(New(maxToolOutput, nil), litTasks)

	statuses, ok := schemaAt(t, tasks.Info().Parameters, litTasks, "items", "properties", litStatus, "enum").([]any)
	require.True(t, ok, "the schema offers no list of statuses")

	require.Len(t, statuses, 4, "want the four statuses")

	for _, offered := range statuses {
		status := asString(t, offered)

		_, err := plan.ParseTasks(taskCall(task("a task", status)))
		require.NoError(t, err, "the schema offers %q but the parser refuses it", status)
	}
}
