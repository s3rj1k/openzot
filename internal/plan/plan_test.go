package plan_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/plan"
	"github.com/openzot/openzot/internal/testutils"
)

func TestParseTasksReadsTitlesStatusesAndNotes(t *testing.T) {
	tasks, err := plan.ParseTasks(testutils.TaskCall(
		testutils.Task("read the parser", "done"),
		map[string]any{litTitle: "  fix the lexer  ", litStatus: "in_progress", "note": " hit in TestLex "},
		testutils.Task("add a test", "pending"),
		map[string]any{litTitle: "deploy", litStatus: "blocked", "note": litNeedsCredentials},
	))
	require.NoError(t, err)

	want := []plan.Task{
		{Title: "read the parser", Status: plan.TaskDone},
		{Title: "fix the lexer", Status: plan.TaskInProgress, Note: "hit in TestLex"},
		{Title: "add a test", Status: plan.TaskPending},
		{Title: "deploy", Status: plan.TaskBlocked, Note: litNeedsCredentials},
	}

	require.Len(t, tasks, len(want))

	for i := range want {
		assert.Equal(t, want[i], tasks[i], "task %d", i+1)
	}
}

// A model listing the work for the first time often leaves the status off. That
// is a pending task, not a reason to reject the list.
func TestATaskWithNoStatusIsPending(t *testing.T) {
	tasks, err := plan.ParseTasks(testutils.TaskCall(map[string]any{litTitle: "write it"}))
	require.NoError(t, err)

	assert.Equal(t, plan.TaskPending, tasks[0].Status)
}

// Each of these is a mistake the model can correct once it is told what it was,
// so each has to be rejected with words that say so.
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
		{"a blank title", testutils.TaskCall(testutils.Task("   ", "pending")), "task 1: a task needs a title"},
		{"a missing title", testutils.TaskCall(map[string]any{litStatus: "done"}), "task 1: a task needs a title"},
		{"an unknown status", testutils.TaskCall(testutils.Task("a", "done"), testutils.Task("b", "started")), `task 2: unknown status "started"`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := plan.ParseTasks(test.args)
			require.Error(t, err)

			assert.Contains(t, err.Error(), test.want)
		})
	}
}

func TestCountDoneCountsOnlyFinishedTasks(t *testing.T) {
	tasks := []plan.Task{
		{Status: plan.TaskDone}, {Status: plan.TaskInProgress}, {Status: plan.TaskDone}, {Status: plan.TaskBlocked}, {Status: plan.TaskPending},
	}

	got := plan.CountDone(tasks)
	assert.Equal(t, 2, got, "only done counts, not in_progress or blocked")

	assert.Equal(t, 0, plan.CountDone(nil), "no tasks is nothing done")
}

func TestTheChecklistMarkersAreDistinct(t *testing.T) {
	seen := map[string]plan.TaskStatus{}

	for _, status := range []plan.TaskStatus{plan.TaskPending, plan.TaskInProgress, plan.TaskDone, plan.TaskBlocked} {
		marker := plan.TaskMarker(status)

		other, dup := seen[marker]
		assert.False(t, dup, "%q and %q share the marker %q", status, other, marker)

		seen[marker] = status
	}
}

// The list reads back headed by how much of it is done, one marked line per task,
// with the note beside it. On a long run the latest result is the one place the
// whole plan is always in view.
func TestFormatTasksReadsTheListBack(t *testing.T) {
	got := plan.FormatTasks([]plan.Task{
		{Title: "read the code", Status: plan.TaskDone},
		{Title: "fix it", Status: plan.TaskInProgress, Note: "the handler"},
		{Title: "ship", Status: plan.TaskBlocked, Note: litNeedsCredentials},
		{Title: "celebrate", Status: plan.TaskPending},
	})

	want := "tasks: 1/4 done\n[x] read the code\n[>] fix it - the handler\n[!] ship - needs credentials\n[ ] celebrate"

	assert.Equal(t, want, got)
}
