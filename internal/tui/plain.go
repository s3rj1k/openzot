package tui

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/mattn/go-isatty"

	"github.com/openzot/openzot/internal/agent"
)

// isInteractive reports whether stdout is a terminal capable of the full-screen
// UI. When it isn't (piped, redirected, run under another process, CI), zot
// falls back to plain mode instead of trying - and failing - to start an
// alt-screen program.
func isInteractive() bool {
	fd := os.Stdout.Fd()
	return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
}

// runPlain streams the agent's activity as plain, unstyled lines. It is the
// explicit ui.plain path; automatic non-TTY output uses runStream so an ANSI-aware
// consumer can opt into colors without becoming an interactive terminal.
func runPlain(ctx context.Context, client *agent.Client, meta Meta, opts agent.ExecuteWithToolsOptions) (Outcome, error) {
	return runStream(ctx, client, meta, opts, false)
}

// streamColorEnabled decides whether a non-interactive transcript may carry
// ANSI. Auto is conservative for pipes and logs; a renderer that understands
// ANSI can opt in explicitly through ui.color or either conventional force
// variable. An explicit Zot mode wins over ambient compatibility variables.
func streamColorEnabled(mode string) bool {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "always":
		return true
	case "never":
		return false
	}
	if noColor, ok := os.LookupEnv("NO_COLOR"); ok && noColor != "" {
		return false
	}
	for _, name := range []string{"FORCE_COLOR", "CLICOLOR_FORCE"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" && value != "0" {
			return true
		}
	}
	return false
}

type streamPalette struct{ enabled bool }

func (p streamPalette) paint(code, text string) string {
	if !p.enabled || text == "" {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

// runStream writes an append-only transcript. Unlike the Bubble Tea viewer it
// never uses an alternate screen, reads input, or advertises keyboard controls.
func runStream(ctx context.Context, client *agent.Client, meta Meta, opts agent.ExecuteWithToolsOptions, color bool) (Outcome, error) {
	palette := streamPalette{enabled: color}

	// The title names the run; the task is what it was actually asked to do.
	// The viewer has one line and must choose, but a transcript is read after
	// the fact - and a log that records only the label loses the brief that
	// explains every line under it.
	if meta.Title != "" {
		fmt.Printf("%s: %s\n", palette.paint("1;94", "zot"), meta.Title)
		fmt.Printf("%s\n", palette.paint("2", meta.Task))
	} else {
		fmt.Printf("%s: %s\n", palette.paint("1;94", "zot"), meta.Task)
	}
	fmt.Printf("%s %s · %s %s · %s %s\n",
		palette.paint("2", "provider"), meta.Provider,
		palette.paint("2", "model"), palette.paint("94", meta.Model),
		palette.paint("2", "dir"), meta.Workdir)

	events, errs := agent.ExecuteWithTools(ctx, client, opts)

	var pending strings.Builder
	var exitErr error
	var sawExit bool
	var outcome Outcome
	flush := func() {
		if s := strings.TrimSpace(pending.String()); s != "" {
			fmt.Printf("  • %s\n", s)
		}
		pending.Reset()
	}

	for ev := range events {
		switch e := ev.(type) {
		case agent.IterationEvent:
			flush()
			fmt.Printf("\n%s\n", palette.paint("2", fmt.Sprintf("── iteration %d ──", e.Iteration)))
		case agent.TokenAgentEvent:
			pending.WriteString(e.Token)
		case agent.ResultAgentEvent:
			flush()
		case agent.ToolCallStartEvent:
			flush()
			fmt.Printf("  %s %s\n", palette.paint("36", e.Name), plainArg(e.Name, e.Args))
		case agent.ToolCallEndEvent:
			if s := plainToolEnd(e.Name, e.Result); s != "" {
				fmt.Println(palette.paint("2", s))
			}
		case agent.ToolCallErrorEvent:
			fmt.Printf("    %s: %s: %s\n", palette.paint("31", "error"), e.Name, e.Error)
		case agent.RetryEvent:
			flush()
			fmt.Printf("  %s %s\n", palette.paint("33", "↻ retrying"), palette.paint("2", e.Error))
		case agent.NoticeEvent:
			flush()
			fmt.Printf("  %s %s\n", palette.paint("33", "⚠"), palette.paint("2", e.Text))
		case agent.AgentExitEvent:
			sawExit = true
			outcome = Outcome{Reason: e.Reason, Message: e.Message}
			flush()
			status := "done"
			if e.Code != 0 {
				// A declared failure is an outcome the model reached, not a
				// harness malfunction: "failed: <reason>" mirrors the viewer's
				// "✗ failed", where an exit code would send the operator
				// hunting for a crash. The exit code itself is unchanged.
				if e.Reason == agent.ReasonFailed {
					status = "failed"
				} else {
					status = fmt.Sprintf("failed (code %d)", e.Code)
				}
				exitErr = &AgentExitError{Code: e.Code, Message: e.Message}
			}
			statusColor := "32"
			if e.Code != 0 {
				statusColor = "31"
			}
			fmt.Printf("\n%s: %s\n", palette.paint(statusColor, status), e.Message)
		}
	}

	if err := <-errs; err != nil {
		return Outcome{}, err
	}
	if !sawExit {
		return Outcome{}, fmt.Errorf("agent stream ended without an exit")
	}
	return outcome, exitErr
}

// plainArg is the one-line argument summary for a tool call.
//
// The names have to match agent.DefaultTools: a mismatch falls through to the
// generic key/value dump, which is how a shell command used to print as
// "shell command=go test" instead of just the command.
func plainArg(name string, args map[string]interface{}) string {
	switch name {
	case "shell":
		return truncate(str(args, "command"), 200)
	case "tasks":
		return plainTasks(args)
	default:
		return compactArgs(args)
	}
}

// plainTasks renders the task list as a checklist on its own lines, so a piped
// run records what the agent set out to do, and how far it got, in full rather
// than as a truncated key/value dump.
func plainTasks(args map[string]interface{}) string {
	tasks, err := agent.ParseTasks(args)
	if err != nil {
		return ""
	}

	var b strings.Builder

	b.WriteString(fmt.Sprintf("%d/%d done", agent.CountDone(tasks), len(tasks)))

	for _, task := range tasks {
		b.WriteString("\n      " + agent.TaskMarker(task.Status) + " " + task.Title)

		if task.Note != "" {
			b.WriteString(" - " + task.Note)
		}
	}

	return b.String()
}

// plainToolEnd summarises a tool result for the unstyled log.
//
// zot's tools return strings, so that is handled first; the map form is kept for
// a caller whose own tool returns something structured.
func plainToolEnd(name string, result interface{}) string {
	if text, ok := result.(string); ok {
		trimmed := strings.TrimRight(text, "\n")

		switch name {
		case "shell":
			if trimmed == "" {
				return ""
			}

			return plainOutput(trimmed)

		default:
			if trimmed == "" {
				return ""
			}

			return plainOutput(trimmed)
		}
	}

	m, ok := result.(map[string]interface{})
	if !ok {
		return ""
	}

	if success, present := m["success"].(bool); present && !success {
		if e := str(m, "error"); e != "" {
			return "    error: " + truncate(e, 200)
		}
	}

	text := strings.TrimRight(str(m, "stdout"), "\n")

	if text == "" {
		text = strings.TrimRight(str(m, "stderr"), "\n")
	}

	if text == "" {
		return ""
	}

	return plainOutput(text)
}

// plainOutput renders captured output, capped so one noisy command cannot bury
// the rest of the run in a log or a CI transcript.
func plainOutput(text string) string {
	lines := strings.Split(text, "\n")

	clipped := false

	if len(lines) > maxOutputLines {
		lines = lines[:maxOutputLines]
		clipped = true
	}

	var b strings.Builder

	for i, l := range lines {
		if i > 0 {
			b.WriteString("\n")
		}

		b.WriteString("    | " + truncate(l, 200))
	}

	if clipped {
		b.WriteString("\n    | ...")
	}

	return b.String()
}
