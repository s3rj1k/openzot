package render

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"

	"github.com/openzot/openzot/internal/plan"
)

// taskMarker is the glyph drawn beside a task, colored for its status.
func taskMarker(status plan.TaskStatus) string {
	switch status {
	case plan.TaskDone:
		return OkStyle.Render("✓")
	case plan.TaskInProgress:
		return toolOtherStyle.Render("▶")
	case plan.TaskBlocked:
		return ErrStyle.Render("✗")
	default:
		return OutputStyle.Render("·")
	}
}

// taskLineStyle dims what is finished and keeps the task being worked on bright,
// so the eye lands on where the run is.
func taskLineStyle(status plan.TaskStatus) lipgloss.Style {
	switch status {
	case plan.TaskDone:
		return OutputStyle
	case plan.TaskBlocked:
		return ErrStyle
	default:
		return TaskStyle
	}
}

// Truncate flattens a string to one line and caps it at limit characters. Characters, not bytes, since slicing bytes
// cuts a multi-byte rune in half, rendering a replacement character and cutting CJK or emoji far short of the width.
func Truncate(s string, limit int) string {
	s = strings.ReplaceAll(s, "\n", " ")

	if utf8.RuneCountInString(s) <= limit {
		return s
	}

	return string([]rune(s)[:limit-1]) + "…"
}

// --- small helpers over the loosely-typed arg/result maps -------------------.

// renderTasks lays the task list out as a checklist, one line per task, headed by how much is done. It is the one piece
// of the run worth reading in full, since it is the map the agent follows and shows the operator whether the approach is sound.
func renderTasks(args map[string]any) string {
	head := toolOtherStyle.Render("  tasks  ")

	tasks, err := plan.ParseTasks(args)
	if err != nil {
		// the call itself is rejected with the reason, so the log has nothing
		// worth drawing beyond the header
		return head
	}

	var b strings.Builder

	b.WriteString(head + OutputStyle.Render(fmt.Sprintf("%d/%d done", plan.CountDone(tasks), len(tasks))))

	for _, task := range tasks {
		b.WriteString("\n    " + taskMarker(task.Status) + " ")
		b.WriteString(taskLineStyle(task.Status).Render(Truncate(task.Title, 200)))

		if task.Note != "" {
			b.WriteString(OutputStyle.Render(" - " + Truncate(task.Note, 160)))
		}
	}

	return b.String()
}

func compactArgs(args map[string]any) string {
	parts := make([]string, 0, len(args))
	for k, v := range args {
		parts = append(parts, fmt.Sprintf("%s=%s", k, Truncate(fmt.Sprint(v), 40)))
	}

	return strings.Join(parts, " ")
}

func str(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}

	return ""
}

func pad(s string, n int) string {
	for len(s) < n {
		s += " "
	}

	return s
}

// RenderToolStart turns a tool invocation into one or more styled log lines. Built-in tools get a tailored form and
// anything else a generic one. The names match the tools package, and a mismatch is no compile error, it just renders
// the most-used tool as an anonymous key/value dump.
func RenderToolStart(name string, args map[string]any) string {
	switch name {
	case "shell":
		return toolExecStyle.Render("  shell  ") + TaskStyle.Render(Truncate(str(args, "command"), 200))
	case "tasks":
		return renderTasks(args)
	default:
		return toolOtherStyle.Render("  "+pad(name, 6)+" ") + OutputStyle.Render(compactArgs(args))
	}
}

// renderOutputLines renders captured output. It does not cap it. How much of a
// record fits is the viewer's call, made against the terminal's height (see
// Model.wrapRecord).
func renderOutputLines(text string) string {
	lines := strings.Split(text, "\n")

	var b strings.Builder

	for i, l := range lines {
		if i > 0 {
			b.WriteString("\n")
		}

		b.WriteString(OutputStyle.Render("    │ " + Truncate(l, 200)))
	}

	return b.String()
}

// renderTextResult summarizes a string result. A shell command's output is what the operator most wants to see, so it
// is echoed, and how much stays on screen is the viewer's call (see model.wrapRecord).
func renderTextResult(name, text string) string {
	trimmed := strings.TrimRight(text, "\n")

	switch name {
	case "shell":
		if trimmed == "" {
			return OkStyle.Render("    ✓ done")
		}

		return OkStyle.Render("    ✓ done") + "\n" + renderOutputLines(trimmed)

	default:
		if trimmed == "" {
			return ""
		}

		return renderOutputLines(trimmed)
	}
}

// CommandOutput renders the stdout/stderr of a structured result.
func CommandOutput(m map[string]any) string {
	text := strings.TrimRight(str(m, "stdout"), "\n")

	if text == "" {
		text = strings.TrimRight(str(m, "stderr"), "\n")
	}

	if text == "" {
		return ""
	}

	return renderOutputLines(text)
}

// RenderToolEnd produces an optional follow-up line summarizing a tool result, or "" when there is nothing worth showing.
// Agent's tools return plain strings, handled first. The map form is for a caller whose own tool returns something structured.
func RenderToolEnd(name string, result any) string {
	if text, ok := result.(string); ok {
		return renderTextResult(name, text)
	}

	m, ok := result.(map[string]any)
	if !ok {
		return ""
	}

	if success, present := m["success"].(bool); present && !success {
		if e := str(m, "error"); e != "" {
			out := ErrStyle.Render("    ✗ " + Truncate(e, 200))
			if tail := CommandOutput(m); tail != "" {
				out += "\n" + tail
			}

			return out
		}
	}

	if tail := CommandOutput(m); tail != "" {
		return OkStyle.Render("    ✓ done") + "\n" + tail
	}

	return OkStyle.Render("    ✓ done")
}

// ShortPath fits a directory into limit columns from the right, since the informative end of a path is the last segment.
// Whole leading segments are dropped and the cut marked with "…/", so a long path reads "…/repos/agent/tool". Only when the
// final segment alone will not fit is it cut, from the left.
func ShortPath(path string, limit int) string {
	if limit <= 0 {
		return ""
	}

	path = strings.ReplaceAll(path, "\n", " ")

	if utf8.RuneCountInString(path) <= limit {
		return path
	}

	segments := strings.Split(strings.TrimRight(path, "/"), "/")

	// grow from the right while the whole thing, plus the "…/" marker, fits
	kept := ""

	for _, candidate := range slices.Backward(segments) {
		if candidate == "" {
			continue
		}

		if kept != "" {
			candidate += "/" + kept
		}

		if utf8.RuneCountInString(candidate)+2 > limit {
			break
		}

		kept = candidate
	}

	// not even the last segment fits whole. Cut it from the left, keeping the
	// end of the name, which is where a project's identity usually lives
	if kept == "" {
		last := segments[len(segments)-1]
		runes := []rune(last)

		if len(runes) > limit-1 {
			runes = runes[len(runes)-(limit-1):]
		}

		return "…" + string(runes)
	}

	return "…/" + kept
}
