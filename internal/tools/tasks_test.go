package tools

import (
	"strings"
	"testing"
)

// taskCall builds the arguments of a call to the tasks tool the way the model
// sends them: JSON-decoded, so lists are []any and objects are map[string]any.
func taskCall(tasks ...map[string]any) map[string]any {
	list := make([]any, 0, len(tasks))

	for _, task := range tasks {
		list = append(list, task)
	}

	return map[string]any{"tasks": list}
}

func task(title, status string) map[string]any {
	return map[string]any{"title": title, "status": status}
}

func TestParseTasksReadsTitlesStatusesAndNotes(t *testing.T) {
	tasks, err := ParseTasks(taskCall(
		task("read the parser", "done"),
		map[string]any{"title": "  fix the lexer  ", "status": "in_progress", "note": " hit in TestLex "},
		task("add a test", "pending"),
		map[string]any{"title": "deploy", "status": "blocked", "note": "needs credentials"},
	))
	if err != nil {
		t.Fatalf("ParseTasks: %v", err)
	}

	want := []Task{
		{Title: "read the parser", Status: TaskDone},
		{Title: "fix the lexer", Status: TaskInProgress, Note: "hit in TestLex"},
		{Title: "add a test", Status: TaskPending},
		{Title: "deploy", Status: TaskBlocked, Note: "needs credentials"},
	}

	if len(tasks) != len(want) {
		t.Fatalf("got %d tasks, want %d: %+v", len(tasks), len(want), tasks)
	}

	for i := range want {
		if tasks[i] != want[i] {
			t.Errorf("task %d = %+v, want %+v", i+1, tasks[i], want[i])
		}
	}
}

// A model listing the work for the first time often leaves the status off. That
// is a pending task, not a reason to refuse the list.
func TestATaskWithNoStatusIsPending(t *testing.T) {
	tasks, err := ParseTasks(taskCall(map[string]any{"title": "write it"}))
	if err != nil {
		t.Fatalf("ParseTasks: %v", err)
	}

	if tasks[0].Status != TaskPending {
		t.Errorf("status = %q, want pending", tasks[0].Status)
	}
}

// Each of these is a mistake the model can correct once it is told what it was,
// so each has to be refused with words that say so.
func TestParseTasksRefusesAMalformedList(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"no tasks argument", map[string]any{}, "at least one task"},
		{"an empty list", map[string]any{"tasks": []any{}}, "at least one task"},
		{"tasks that is not a list", map[string]any{"tasks": "do it"}, "at least one task"},
		{"a task that is not an object", map[string]any{"tasks": []any{"do it"}}, "task 1: expected an object"},
		{"a blank title", taskCall(task("   ", "pending")), "task 1: a task needs a title"},
		{"a missing title", taskCall(map[string]any{"status": "done"}), "task 1: a task needs a title"},
		{"an unknown status", taskCall(task("a", "done"), task("b", "started")), `task 2: unknown status "started"`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseTasks(test.args)
			if err == nil {
				t.Fatal("expected an error")
			}

			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("error = %q, want it to say %q", err, test.want)
			}
		})
	}
}

func TestCountDoneCountsOnlyFinishedTasks(t *testing.T) {
	tasks := []Task{
		{Status: TaskDone}, {Status: TaskInProgress}, {Status: TaskDone}, {Status: TaskBlocked}, {Status: TaskPending},
	}

	if got := CountDone(tasks); got != 2 {
		t.Errorf("CountDone = %d, want 2: only done counts, not in_progress or blocked", got)
	}

	if CountDone(nil) != 0 {
		t.Error("no tasks is nothing done")
	}
}

// The tool answers with the list, so the model reads its own state back on every
// call - the latest result is the one place the whole list is always in view.
func TestTheTasksToolAnswersWithTheChecklist(t *testing.T) {
	got, err := call(t, DefaultTools(), "tasks", taskCall(
		task("read the parser", "done"),
		map[string]any{"title": "fix the lexer", "status": "in_progress", "note": "hit in TestLex"},
		task("add a test", "pending"),
		map[string]any{"title": "deploy", "status": "blocked", "note": "needs credentials"},
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
// the tool, so a second call is never merged into the first: a task left out of
// it is a task that is no longer wanted.
func TestTasksReplaceRatherThanMerge(t *testing.T) {
	tools := DefaultTools()

	if _, err := call(t, tools, "tasks", taskCall(task("first", "pending"), task("second", "pending"))); err != nil {
		t.Fatalf("first call: %v", err)
	}

	got, err := call(t, tools, "tasks", taskCall(task("third", "in_progress")))
	if err != nil {
		t.Fatalf("second call: %v", err)
	}

	text := got.(string)

	if !strings.Contains(text, "0/1 done") || !strings.Contains(text, "third") {
		t.Errorf("the second list should stand alone:\n%s", text)
	}

	if strings.Contains(text, "first") || strings.Contains(text, "second") {
		t.Errorf("the second call kept tasks from the first:\n%s", text)
	}
}

func TestTheTasksToolRefusesAMalformedList(t *testing.T) {
	if _, err := call(t, DefaultTools(), "tasks", map[string]any{"tasks": []any{}}); err == nil {
		t.Error("an empty list must be refused")
	}

	if _, err := call(t, DefaultTools(), "tasks", taskCall(task("a", "finished"))); err == nil {
		t.Error("an unknown status must be refused")
	}
}

// What the model is told the tool accepts has to be what the tool accepts. The
// schema names the statuses, so a status the schema offers but the parser
// refuses would be a trap.
func TestTheSchemaOffersOnlyStatusesTheParserAccepts(t *testing.T) {
	tasks, _ := findTool(DefaultTools(), "tasks")

	items := tasks.Info().Parameters["tasks"].(map[string]any)["items"].(map[string]any)
	statuses := items["properties"].(map[string]any)["status"].(map[string]any)["enum"].([]any)

	if len(statuses) != 4 {
		t.Fatalf("schema offers %v, want the four statuses", statuses)
	}

	for _, offered := range statuses {
		status := offered.(string)

		if _, err := ParseTasks(taskCall(task("a task", status))); err != nil {
			t.Errorf("the schema offers %q but the parser refuses it: %v", status, err)
		}
	}
}

func TestTheChecklistMarkersAreDistinct(t *testing.T) {
	seen := map[string]TaskStatus{}

	for _, status := range []TaskStatus{TaskPending, TaskInProgress, TaskDone, TaskBlocked} {
		marker := TaskMarker(status)

		if other, dup := seen[marker]; dup {
			t.Errorf("%q and %q share the marker %q", status, other, marker)
		}

		seen[marker] = status
	}
}
