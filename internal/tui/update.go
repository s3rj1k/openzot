package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/openzot/openzot/internal/loop"
	"github.com/openzot/openzot/internal/outcome"
	"github.com/openzot/openzot/internal/render"
)

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return TickMsg{} })
}

func (m *Model) AppendEntry(s string) {
	m.Entries = append(m.Entries, s)

	// The buffer may grow a quarter past the cap before trimming, so the linear re-wrap a trim costs is
	// amortized over many appends instead of paid on every one once the cap is reached.
	slack := m.MaxEntries / 4

	if len(m.Entries) > m.MaxEntries+slack {
		// Keep the most recent m.MaxEntries, copied into a fresh slice so the old backing array is released,
		// then rebuild the wrapped cache from the trimmed set.
		kept := make([]string, m.MaxEntries)
		copy(kept, m.Entries[len(m.Entries)-m.MaxEntries:])
		m.Entries = kept
		m.Truncated = true
		m.rewrap()

		return
	}

	// Append only the new entry to the cache. Wrapping per entry equals wrapping the joined buffer, since
	// the wrap is per line, so per-append cost does not grow with the length of the run.
	wrapped := m.wrapRecord(s)
	if m.CommittedWrapped == "" {
		m.CommittedWrapped = wrapped
	} else {
		m.CommittedWrapped += "\n" + wrapped
	}

	m.Render()
}

// FlushPending commits any streamed assistant narration as a dim thought block.
func (m *Model) FlushPending() {
	text := strings.TrimSpace(m.pending)
	m.pending = ""

	if text == "" {
		return
	}

	m.AppendEntry(render.ThoughtStyle.Render("  ◆ " + text))
}

// HandleEvent folds one event of the run into the UI state.
func (m *Model) HandleEvent(ev *loop.Event) {
	switch ev.Kind {
	case loop.EventIteration:
		m.Iteration = ev.Iteration
		m.FlushPending()
		// A fixed short rule. One that fills the width would wrap at a narrow
		// terminal and smear the divider across two rows.
		m.AppendEntry(render.DividerStyle.Render(fmt.Sprintf("─── iteration %d ───", ev.Iteration)))

	case loop.EventToken:
		m.pending += ev.Text
		m.Render()

	case loop.EventToolCallStart:
		m.FlushPending()
		m.AppendEntry(render.RenderToolStart(ev.Tool, ev.Args))

	case loop.EventToolCallEnd:
		if s := render.RenderToolEnd(ev.Tool, ev.Result); s != "" {
			m.AppendEntry(s)
		}

	case loop.EventToolCallError:
		m.AppendEntry(render.ErrStyle.Render("    ✗ " + ev.Tool + ": " + ev.Text))

	case loop.EventNotice:
		// A corrective nudge (empty turn, truncation continuation, settle reminder). Without this line the
		// recovery renders as bare iteration dividers, indistinguishable from a hang.
		m.FlushPending()
		m.AppendEntry(render.StatusRunningStyle.Render("⚠ ") + render.MetaStyle.Render(ev.Text))

	case loop.EventRetry:
		// A retried provider failure spends a continuation and waits out a backoff. Without this line the wait
		// renders as empty iterations stacking up, so a surviving run looks like a hanging one.
		m.FlushPending()
		m.AppendEntry(render.StatusRunningStyle.Render("↻ retrying") + "  " + render.MetaStyle.Render(ev.Text))

	case loop.EventUsage:
		// provider-reported cumulative token usage, shown in the meta bar
		m.InputTokens = ev.InputTokens
		m.OutputTokens = ev.OutputTokens

	default:
		// reasoning, whole messages and a runaway cut are for the log. The
		// tokens and the notices already show what they say
	}
}

// Finish folds the run's ending into the UI state, the conclusion and the error behind it. The error is usually the
// run's only diagnostic, so it is kept and shown.
func (m *Model) Finish(result *loop.Result) {
	code := result.ExitCode()

	m.exitCode = code
	m.exitReason = string(result.Reason)
	m.ExitMsg = result.Message
	m.FlushPending()

	switch {
	case code == 0:
		m.Status = StatusDone
		m.AppendEntry("\n" + render.OkStyle.Render("✓ done") + "  " + render.TaskStyle.Render(result.Message))

	case result.Reason == outcome.StopFailed:
		// the model reached a conclusion and the conclusion is "no" - an
		// outcome, not a malfunction, so it does not get a process exit code
		m.Status = StatusFailed
		m.AppendEntry("\n" + render.ErrStyle.Render("✗ failed") + "  " + render.TaskStyle.Render(result.Message))

	default:
		m.Status = StatusFailed
		m.AppendEntry("\n" + render.ErrStyle.Render(fmt.Sprintf("✗ exited (code %d)", code)) + "  " + render.TaskStyle.Render(result.Message))
	}

	if result.Err != nil && m.Err == nil {
		m.Err = result.Err
		m.AppendEntry(render.ErrStyle.Render("✗ " + result.Err.Error()))
	}
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.Width, m.Height = msg.Width, msg.Height

		vpHeight := max(msg.Height-reserved, 1)
		if !m.Ready {
			m.Viewport = viewport.New(msg.Width, vpHeight)
			m.Ready = true
		} else {
			m.Viewport.Width = msg.Width
			m.Viewport.Height = vpHeight
		}

		m.rewrap()

		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "g", "home":
			m.Viewport.GotoTop()
			m.Follow = false

			return m, nil
		case "G", "end":
			m.Viewport.GotoBottom()
			m.Follow = true

			return m, nil
		}

		var cmd tea.Cmd

		m.Viewport, cmd = m.Viewport.Update(msg)
		m.Follow = m.Viewport.AtBottom()

		return m, cmd

	case spinner.TickMsg:
		if m.Status != StatusRunning {
			return m, nil
		}

		var cmd tea.Cmd

		m.spinner, cmd = m.spinner.Update(msg)

		return m, cmd

	case TickMsg:
		if m.Status != StatusRunning {
			return m, nil
		}

		m.Elapsed = time.Since(m.startedAt)

		return m, tickCmd()

	case EventMsg:
		m.HandleEvent(&msg.Event)
		return m, nil

	case DoneMsg:
		m.Finish(&msg.Result)
		return m, nil
	}

	return m, nil
}
