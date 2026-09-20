// Package plan is the model's task list: what a call to the plan tool means, how
// it is read, and how it reads back. The tools package wraps it as a tool and the
// viewer draws it, and neither has to know the other to agree on the schema.
package plan

import (
	"errors"
	"fmt"
	"strings"
)

// Tool is the name of the tool that keeps the run's plan.
const Tool = "tasks"

// TaskStatus is where one task stands.
type TaskStatus string

const (
	// TaskPending is work not yet started.
	TaskPending TaskStatus = "pending"

	// TaskInProgress is the task being worked on now.
	TaskInProgress TaskStatus = "in_progress"

	// TaskDone is finished work.
	TaskDone TaskStatus = "done"

	// TaskBlocked is work that cannot go on until something changes; its note
	// says what.
	TaskBlocked TaskStatus = "blocked"
)

// Task is one thing the work needs, and how far along it is.
type Task struct {
	// Title says what the task is.
	Title string

	// Status is where the task stands. Empty on input means pending.
	Status TaskStatus

	// Note is free text beside the task: why it is blocked, what was found, or
	// an assumption made in doing it.
	Note string
}

// ParseTasks reads the task list out of a call to the tasks tool.
//
// It is the one place the schema is read. The handler, the viewer and the
// viewer's task count all come through it, so what counts as a valid list cannot
// drift between what the model is told and what the operator is shown. A task
// with no status is pending; anything else that is not a status is an error the
// model can act on.
func ParseTasks(args map[string]any) ([]Task, error) {
	raw, _ := args["tasks"].([]any)

	if len(raw) == 0 {
		return nil, errors.New("tasks needs at least one task")
	}

	tasks := make([]Task, 0, len(raw))

	for i, entry := range raw {
		fields, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("task %d: expected an object with a title and a status", i+1)
		}

		title, _ := fields["title"].(string)
		title = strings.TrimSpace(title)

		if title == "" {
			return nil, fmt.Errorf("task %d: a task needs a title", i+1)
		}

		status := TaskStatus(strings.TrimSpace(stringField(fields, "status")))

		switch status {
		case "":
			status = TaskPending
		case TaskPending, TaskInProgress, TaskDone, TaskBlocked:
		default:
			return nil, fmt.Errorf("task %d: unknown status %q (use pending, in_progress, done or blocked)", i+1, status)
		}

		tasks = append(tasks, Task{
			Title:  title,
			Status: status,
			Note:   strings.TrimSpace(stringField(fields, "note")),
		})
	}

	return tasks, nil
}

// CountDone is how many of the tasks are finished.
func CountDone(tasks []Task) int {
	done := 0

	for _, task := range tasks {
		if task.Status == TaskDone {
			done++
		}
	}

	return done
}

// FormatTasks renders the list as a checklist, headed by how much of it is done.
//
// This is what the tool answers with, so the model reads its own state back on
// every call: on a long run the older messages have been trimmed away, and the
// latest result is the one place the whole list is always in view.
func FormatTasks(tasks []Task) string {
	var b strings.Builder

	fmt.Fprintf(&b, "tasks: %d/%d done", CountDone(tasks), len(tasks))

	for _, task := range tasks {
		b.WriteString("\n" + TaskMarker(task.Status) + " " + task.Title)

		if task.Note != "" {
			b.WriteString(" - " + task.Note)
		}
	}

	return b.String()
}

// TaskMarker is the plain-text box drawn beside a task of the given status.
func TaskMarker(status TaskStatus) string {
	switch status {
	case TaskDone:
		return "[x]"
	case TaskInProgress:
		return "[>]"
	case TaskBlocked:
		return "[!]"
	default:
		return "[ ]"
	}
}

// stringField reads a string field, and is empty for anything that is not one.
func stringField(fields map[string]any, key string) string {
	value, _ := fields[key].(string)

	return value
}
