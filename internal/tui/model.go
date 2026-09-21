package tui

import (
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/openzot/openzot/internal/render"
)

type Status int

const (
	StatusRunning Status = iota
	StatusDone
	StatusFailed
)

// reserved counts the non-viewport rows. Title + meta + a blank gap + footer.
const reserved = 4

// TickMsg drives the elapsed-time clock once a second while the agent runs.
type TickMsg struct{}

// Model is the entire read-only UI. It holds no input field by design. The user
// watches, they do not type. Everything it shows is derived from the agent's
// event stream plus a couple of counters.
type Model struct {
	Task     string
	Title    string // shown instead of task when set - see tui.Meta.Title
	model    string
	provider string
	Workdir  string

	spinner  spinner.Model
	Viewport viewport.Model
	Ready    bool
	Width    int
	Height   int

	// Activity log. entries are the committed logical lines, CommittedWrapped caches them word-wrapped to
	// the width so per-token redraws stay cheap, and pending holds the assistant's in-flight narration.
	Entries          []string
	CommittedWrapped string
	pending          string
	Follow           bool // auto-scroll to the newest activity
	Truncated        bool // oldest lines have been dropped to bound memory
	MaxEntries       int  // scrollback cap (DefaultMaxScrollback unless overridden)

	Status     Status
	Iteration  int
	exitCode   int
	exitReason string
	ExitMsg    string
	Err        error

	// Provider-reported cumulative token usage (not a local estimate).
	InputTokens  int
	OutputTokens int

	// Configured limits, for the "5/1000" progress display. Zero means the limit
	// is unbounded (or the caller chose not to show it), so no denominator shows.
	MaxIterations int
	MaxDuration   time.Duration

	startedAt time.Time
	Elapsed   time.Duration
}

// --- viewport content management --------------------------------------------.

// DefaultMaxScrollback is the on-screen log cap used when a caller sets none (Meta.MaxScrollback). An unbounded run
// would otherwise grow the viewer's memory without limit, so the oldest lines are dropped at the cap. The full run
// is always in the session log.
const DefaultMaxScrollback = 5000

func NewModel(task, modelName, provider, workdir string) *Model {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(render.ColYellow)

	return &Model{
		Task:       task,
		model:      modelName,
		provider:   provider,
		Workdir:    workdir,
		spinner:    sp,
		Status:     StatusRunning,
		Follow:     true,
		startedAt:  time.Now(),
		MaxEntries: DefaultMaxScrollback,
	}
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, tickCmd())
}

func (m *Model) wrap(s string) string {
	if s == "" || m.Viewport.Width <= 0 {
		return s
	}

	return lipgloss.NewStyle().Width(m.Viewport.Width).Render(s)
}

// recordHeight is the most rows one log record may take, a third of the terminal's height, so no single record can push
// the rest of the run off the screen. Zero, meaning no limit, until the terminal has reported a size.
func (m *Model) recordHeight() int {
	if m.Height <= 0 {
		return 0
	}

	return max(m.Height/3, 2)
}

// wrapRecord wraps one log record to the viewport width and cuts it at
// recordHeight rows, the last of which is an ellipsis when anything was dropped.
// The full run is always in the session log.
func (m *Model) wrapRecord(s string) string {
	limit := m.recordHeight()
	if limit == 0 {
		return m.wrap(s)
	}

	// Every source line takes at least one row, so lines past the limit can never
	// be shown. Dropping them first keeps a huge output from being wrapped whole.
	source := strings.Split(s, "\n")
	cut := len(source) > limit

	if cut {
		source = source[:limit]
	}

	rows := strings.Split(m.wrap(strings.Join(source, "\n")), "\n")

	if len(rows) > limit {
		cut = true
	}

	if !cut {
		return strings.Join(rows, "\n")
	}

	return strings.Join(rows[:limit-1], "\n") + "\n" + render.OutputStyle.Render("    …")
}

// Render pushes the current committed log plus any in-flight narration into the
// viewport, keeping the latest activity in view when following.
func (m *Model) Render() {
	if !m.Ready {
		return
	}

	body := m.CommittedWrapped
	if m.Truncated {
		marker := m.wrap(render.DividerStyle.Render("  ⋮ earlier activity trimmed — the full run is in the session log"))
		if body != "" {
			body = marker + "\n" + body
		} else {
			body = marker
		}
	}

	if p := strings.TrimSpace(m.pending); p != "" {
		if body != "" {
			body += "\n"
		}

		body += m.wrapRecord(render.ThoughtStyle.Render("  ◆ " + p))
	}

	m.Viewport.SetContent(body)

	if m.Follow {
		m.Viewport.GotoBottom()
	}
}

// rewrap recomputes the cached content for a new width.
func (m *Model) rewrap() {
	wrapped := make([]string, len(m.Entries))
	for i, entry := range m.Entries {
		wrapped[i] = m.wrapRecord(entry)
	}

	m.CommittedWrapped = strings.Join(wrapped, "\n")
	m.Render()
}

// --- view -------------------------------------------------------------------.

func (m *Model) View() string {
	if !m.Ready {
		return "starting agent…"
	}

	return strings.Join([]string{
		m.TitleBar(),
		m.MetaBar(),
		"",
		m.Viewport.View(),
		m.Footer(),
	}, "\n")
}
