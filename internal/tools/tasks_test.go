package tools

import (
	"strings"
	"testing"

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
	if err != nil {
		t.Fatalf("tasks: %v", err)
	}

	want := "tasks: 1/4 done\n" +
		"[x] read the parser\n" +
		"[>] fix the lexer - hit in TestLex\n" +
		"[ ] add a test\n" +
		"[!] deploy - needs credentials"

	if got != want {
		t.Errorf("tasks answered:\n%s\nwant:\n%s", got, want)
	}
}

// Every call carries the whole list and replaces the last. There is no state in
// the tool, so a second call is never merged into the first. A task left out of
// it is a task that is no longer wanted.
func TestTasksReplaceRatherThanMerge(t *testing.T) {
	tools := New(maxToolOutput, nil)

	if _, err := call(t, tools, litTasks, taskCall(task("first", "pending"), task("second", "pending"))); err != nil {
		t.Fatalf("first call: %v", err)
	}

	got, err := call(t, tools, litTasks, taskCall(task("third", "in_progress")))
	if err != nil {
		t.Fatalf("second call: %v", err)
	}

	text := asString(t, got)

	if !strings.Contains(text, "0/1 done") || !strings.Contains(text, "third") {
		t.Errorf("the second list should stand alone:\n%s", text)
	}

	if strings.Contains(text, "first") || strings.Contains(text, "second") {
		t.Errorf("the second call kept tasks from the first:\n%s", text)
	}
}

func TestTheTasksToolRefusesAMalformedList(t *testing.T) {
	if _, err := call(t, New(maxToolOutput, nil), litTasks, map[string]any{litTasks: []any{}}); err == nil {
		t.Error("an empty list must be refused")
	}

	if _, err := call(t, New(maxToolOutput, nil), litTasks, taskCall(task("a", "finished"))); err == nil {
		t.Error("an unknown status must be refused")
	}
}

// schemaAt follows a path through a tool's parameter schema.
func schemaAt(t *testing.T, schema map[string]any, path ...string) any {
	t.Helper()

	var node any = schema

	for _, key := range path {
		fields, ok := node.(map[string]any)
		if !ok {
			t.Fatalf("the schema has no object at %q on the way to %v", key, path)
		}

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
	if !ok {
		t.Fatal("the schema offers no list of statuses")
	}

	if len(statuses) != 4 {
		t.Fatalf("schema offers %v, want the four statuses", statuses)
	}

	for _, offered := range statuses {
		status := asString(t, offered)

		if _, err := plan.ParseTasks(taskCall(task("a task", status))); err != nil {
			t.Errorf("the schema offers %q but the parser refuses it: %v", status, err)
		}
	}
}
