package tui

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"

	"github.com/openzot/openzot/internal/agent"
)

// renderToolStart turns a tool invocation into one or more styled log lines.
//
// The built-in tools each get a tailored, scannable representation; anything a
// caller has added falls through to a generic one. The names here are the names
// in agent.DefaultTools - a mismatch is not a compile error, it just quietly
// renders the agent's most-used tool as an anonymous key/value dump.
func renderToolStart(name string, args map[string]interface{}) string {
	switch name {
	case "shell":
		return toolExecStyle.Render("  shell  ") + taskStyle.Render(truncate(str(args, "command"), 200))
	case "tasks":
		return renderTasks(args)
	default:
		return toolOtherStyle.Render("  "+pad(name, 6)+" ") + outputStyle.Render(compactArgs(args))
	}
}

// renderToolEnd produces an optional follow-up line summarising a tool result.
// It returns "" when there is nothing worth showing.
//
// zot's tools return plain strings, so that is the case handled first; the map
// form is kept for a caller whose own tool returns something structured.
func renderToolEnd(name string, result interface{}) string {
	if text, ok := result.(string); ok {
		return renderTextResult(name, text)
	}

	m, ok := result.(map[string]interface{})
	if !ok {
		return ""
	}

	if success, present := m["success"].(bool); present && !success {
		if e := str(m, "error"); e != "" {
			out := errStyle.Render("    ✗ " + truncate(e, 200))
			if tail := commandOutput(m); tail != "" {
				out += "\n" + tail
			}
			return out
		}
	}

	if tail := commandOutput(m); tail != "" {
		return okStyle.Render("    ✓ done") + "\n" + tail
	}

	return okStyle.Render("    ✓ done")
}

// renderTextResult summarises a string result.
//
// A shell command's output is the thing the operator most wants to see, so it is
// echoed; how much of it stays on screen is the viewer's call (see
// model.wrapRecord).
func renderTextResult(name string, text string) string {
	trimmed := strings.TrimRight(text, "\n")

	switch name {
	case "shell":
		if trimmed == "" {
			return okStyle.Render("    ✓ done")
		}

		return okStyle.Render("    ✓ done") + "\n" + renderOutputLines(trimmed)

	default:
		if trimmed == "" {
			return ""
		}

		return renderOutputLines(trimmed)
	}
}

// renderOutputLines renders captured output. It does not cap it: how much of a
// record fits is the viewer's call, made against the terminal's height (see
// model.wrapRecord).
func renderOutputLines(text string) string {
	lines := strings.Split(text, "\n")

	var b strings.Builder

	for i, l := range lines {
		if i > 0 {
			b.WriteString("\n")
		}

		b.WriteString(outputStyle.Render("    │ " + truncate(l, 200)))
	}

	return b.String()
}

// commandOutput renders the stdout/stderr of a structured result.
func commandOutput(m map[string]interface{}) string {
	text := strings.TrimRight(str(m, "stdout"), "\n")

	if text == "" {
		text = strings.TrimRight(str(m, "stderr"), "\n")
	}

	if text == "" {
		return ""
	}

	return renderOutputLines(text)
}

// --- small helpers over the loosely-typed arg/result maps -------------------

// renderTasks lays the task list out as a checklist, one line per task, headed by
// how much of it is done. The list is the one piece of the run worth reading in
// full - it is the map the agent is following and how far along it is, and seeing
// it is how the operator knows whether the approach is sound.
func renderTasks(args map[string]interface{}) string {
	head := toolOtherStyle.Render("  tasks  ")

	tasks, err := agent.ParseTasks(args)
	if err != nil {
		// the call itself is refused with the reason, so the log has nothing
		// worth drawing beyond the header
		return head
	}

	var b strings.Builder

	b.WriteString(head + outputStyle.Render(fmt.Sprintf("%d/%d done", agent.CountDone(tasks), len(tasks))))

	for _, task := range tasks {
		b.WriteString("\n    " + taskMarker(task.Status) + " ")
		b.WriteString(taskLineStyle(task.Status).Render(truncate(task.Title, 200)))

		if task.Note != "" {
			b.WriteString(outputStyle.Render(" - " + truncate(task.Note, 160)))
		}
	}

	return b.String()
}

// taskMarker is the glyph drawn beside a task, coloured for its status.
func taskMarker(status agent.TaskStatus) string {
	switch status {
	case agent.TaskDone:
		return okStyle.Render("✓")
	case agent.TaskInProgress:
		return toolOtherStyle.Render("▶")
	case agent.TaskBlocked:
		return errStyle.Render("✗")
	default:
		return outputStyle.Render("·")
	}
}

// taskLineStyle dims what is finished and keeps the task being worked on bright,
// so the eye lands on where the run is.
func taskLineStyle(status agent.TaskStatus) lipgloss.Style {
	switch status {
	case agent.TaskDone:
		return outputStyle
	case agent.TaskBlocked:
		return errStyle
	default:
		return taskStyle
	}
}

func compactArgs(args map[string]interface{}) string {
	parts := make([]string, 0, len(args))
	for k, v := range args {
		parts = append(parts, fmt.Sprintf("%s=%s", k, truncate(fmt.Sprint(v), 40)))
	}
	return strings.Join(parts, " ")
}

func str(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// truncate flattens a string to one line and caps it at max characters.
//
// Characters, not bytes: slicing bytes cuts a multi-byte rune in half, so a task
// or tool argument in CJK or emoji rendered a replacement character - and the
// cap bit far earlier than the width it was given, since one glyph can be four
// bytes.
func truncate(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")

	if utf8.RuneCountInString(s) <= max {
		return s
	}

	return string([]rune(s)[:max-1]) + "…"
}

func pad(s string, n int) string {
	for len(s) < n {
		s += " "
	}
	return s
}

// shortPath fits a directory into max columns from the right, because the
// informative end of a path is the last segment, not the first.
//
// truncate keeps the head, which for /workspaces/monorepo-zot/repos/zot/tool
// yields "/workspaces/monorepo-zot/repos/z…" - every character spent on the
// part shared by every project on the machine, and the one word naming this one
// cut off. This drops whole leading segments instead and marks the cut with a
// leading "…/", so the same path reads "…/repos/zot/tool".
//
// Segments are kept whole: half a directory name is not a directory name, and a
// path is read by recognising its parts. Only when the final segment alone will
// not fit is it cut, and then from the left, so the end of the name survives.
func shortPath(path string, max int) string {
	if max <= 0 {
		return ""
	}

	path = strings.ReplaceAll(path, "\n", " ")

	if utf8.RuneCountInString(path) <= max {
		return path
	}

	segments := strings.Split(strings.TrimRight(path, "/"), "/")

	// grow from the right while the whole thing, plus the "…/" marker, fits
	kept := ""

	for i := len(segments) - 1; i >= 0; i-- {
		if segments[i] == "" {
			continue
		}

		candidate := segments[i]
		if kept != "" {
			candidate += "/" + kept
		}

		if utf8.RuneCountInString(candidate)+2 > max {
			break
		}

		kept = candidate
	}

	// not even the last segment fits whole: cut it from the left, keeping the
	// end of the name, which is where a project's identity usually lives
	if kept == "" {
		last := segments[len(segments)-1]
		runes := []rune(last)

		if len(runes) > max-1 {
			runes = runes[len(runes)-(max-1):]
		}

		return "…" + string(runes)
	}

	return "…/" + kept
}
