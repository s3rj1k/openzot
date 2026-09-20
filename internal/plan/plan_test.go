package plan

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

	return map[string]any{litTasks: list}
}

func task(title, status string) map[string]any {
	return map[string]any{litTitle: title, litStatus: status}
}

func TestParseTasksReadsTitlesStatusesAndNotes(t *testing.T) {
	tasks, err := ParseTasks(taskCall(
		task("read the parser", "done"),
		map[string]any{litTitle: "  fix the lexer  ", litStatus: "in_progress", "note": " hit in TestLex "},
		task("add a test", "pending"),
		map[string]any{litTitle: "deploy", litStatus: "blocked", "note": litNeedsCredentials},
	))
	if err != nil {
		t.Fatalf("ParseTasks: %v", err)
	}

	want := []Task{
		{Title: "read the parser", Status: TaskDone},
		{Title: "fix the lexer", Status: TaskInProgress, Note: "hit in TestLex"},
		{Title: "add a test", Status: TaskPending},
		{Title: "deploy", Status: TaskBlocked, Note: litNeedsCredentials},
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
	tasks, err := ParseTasks(taskCall(map[string]any{litTitle: "write it"}))
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
		{"no tasks argument", map[string]any{}, litAtLeastOneTask},
		{"an empty list", map[string]any{litTasks: []any{}}, litAtLeastOneTask},
		{"tasks that is not a list", map[string]any{litTasks: "do it"}, litAtLeastOneTask},
		{"a task that is not an object", map[string]any{litTasks: []any{"do it"}}, "task 1: expected an object"},
		{"a blank title", taskCall(task("   ", "pending")), "task 1: a task needs a title"},
		{"a missing title", taskCall(map[string]any{litStatus: "done"}), "task 1: a task needs a title"},
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

// The list reads back headed by how much of it is done, one marked line per task,
// with the note beside it: on a long run the latest result is the one place the
// whole plan is always in view.
func TestFormatTasksReadsTheListBack(t *testing.T) {
	got := FormatTasks([]Task{
		{Title: "read the code", Status: TaskDone},
		{Title: "fix it", Status: TaskInProgress, Note: "the handler"},
		{Title: "ship", Status: TaskBlocked, Note: litNeedsCredentials},
		{Title: "celebrate", Status: TaskPending},
	})

	want := "tasks: 1/4 done\n[x] read the code\n[>] fix it - the handler\n[!] ship - needs credentials\n[ ] celebrate"

	if got != want {
		t.Errorf("FormatTasks =\n%s\nwant\n%s", got, want)
	}
}
