package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/openzot/openzot/internal/loop"
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
	sp.Style = lipgloss.NewStyle().Foreground(colYellow)

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

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return TickMsg{} })
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

	return strings.Join(rows[:limit-1], "\n") + "\n" + outputStyle.Render("    …")
}

// Render pushes the current committed log plus any in-flight narration into the
// viewport, keeping the latest activity in view when following.
func (m *Model) Render() {
	if !m.Ready {
		return
	}

	body := m.CommittedWrapped
	if m.Truncated {
		marker := m.wrap(dividerStyle.Render("  ⋮ earlier activity trimmed — the full run is in the session log"))
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

		body += m.wrapRecord(thoughtStyle.Render("  ◆ " + p))
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

	m.AppendEntry(thoughtStyle.Render("  ◆ " + text))
}

// HandleEvent folds one event of the run into the UI state.
func (m *Model) HandleEvent(ev *loop.Event) {
	switch ev.Kind {
	case loop.EventIteration:
		m.Iteration = ev.Iteration
		m.FlushPending()
		// A fixed short rule. One that fills the width would wrap at a narrow
		// terminal and smear the divider across two rows.
		m.AppendEntry(dividerStyle.Render(fmt.Sprintf("─── iteration %d ───", ev.Iteration)))

	case loop.EventToken:
		m.pending += ev.Text
		m.Render()

	case loop.EventToolCallStart:
		m.FlushPending()
		m.AppendEntry(RenderToolStart(ev.Tool, ev.Args))

	case loop.EventToolCallEnd:
		if s := RenderToolEnd(ev.Tool, ev.Result); s != "" {
			m.AppendEntry(s)
		}

	case loop.EventToolCallError:
		m.AppendEntry(errStyle.Render("    ✗ " + ev.Tool + ": " + ev.Text))

	case loop.EventNotice:
		// A corrective nudge (empty turn, truncation continuation, settle reminder). Without this line the
		// recovery renders as bare iteration dividers, indistinguishable from a hang.
		m.FlushPending()
		m.AppendEntry(statusRunningStyle.Render("⚠ ") + metaStyle.Render(ev.Text))

	case loop.EventRetry:
		// A retried provider failure spends a continuation and waits out a backoff. Without this line the wait
		// renders as empty iterations stacking up, so a surviving run looks like a hanging one.
		m.FlushPending()
		m.AppendEntry(statusRunningStyle.Render("↻ retrying") + "  " + metaStyle.Render(ev.Text))

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
		m.AppendEntry("\n" + okStyle.Render("✓ done") + "  " + taskStyle.Render(result.Message))

	case result.Reason == loop.StopFailed:
		// the model reached a conclusion and the conclusion is "no" - an
		// outcome, not a malfunction, so it does not get a process exit code
		m.Status = StatusFailed
		m.AppendEntry("\n" + errStyle.Render("✗ failed") + "  " + taskStyle.Render(result.Message))

	default:
		m.Status = StatusFailed
		m.AppendEntry("\n" + errStyle.Render(fmt.Sprintf("✗ exited (code %d)", code)) + "  " + taskStyle.Render(result.Message))
	}

	if result.Err != nil && m.Err == nil {
		m.Err = result.Err
		m.AppendEntry(errStyle.Render("✗ " + result.Err.Error()))
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

func (m *Model) Badge() string {
	switch m.Status {
	case StatusDone:
		return statusDoneStyle.Render("✓ done")
	case StatusFailed:
		return statusFailStyle.Render("✗ failed")
	default:
		// Keep the spinner and label as separate same-color pieces. Nesting the
		// spinner's own ANSI inside another style breaks the run of color.
		return m.spinner.View() + statusRunningStyle.Render("working")
	}
}

func (m *Model) TitleBar() string {
	left := titleStyle.Render("✦ zot") + " " + m.Badge()

	room := m.Width - lipgloss.Width(left) - 2
	if room < 8 {
		return left
	}
	// A title is what the header wants. The task is the whole order rendered
	// for the model, so a one-line header of it is a paragraph cut mid-word.
	label := m.Title
	if label == "" {
		label = m.Task
	}

	return left + " " + taskStyle.Render(Truncate(label, room))
}

// cell pads v on the right to at least width columns, so a value that changes length keeps the segments after it
// where they were. The value stays flush against its label, and one wider than the cell is returned whole.
func cell(v string, width int) string {
	if pad := width - lipgloss.Width(v); pad > 0 {
		return v + strings.Repeat(" ", pad)
	}

	return v
}

// FmtTokens renders a token count compactly. 532, 45.2k, 1.2M.
func FmtTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return strconv.Itoa(n)
	}
}

func FmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	return fmt.Sprintf("%02d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}

// MetaBar is the header, provider, model, iteration, elapsed time, tokens and directory, in that order. The bar drops
// what does not fit, so what comes first survives a narrow terminal, and dir is last because it never changes.
func (m *Model) MetaBar() string {
	seg := func(k, v string, value lipgloss.Style) string {
		return metaKey.Render(k+" ") + value.Render(v)
	}

	// iterations renders "n" or "n/max" when a limit is set, so progress against a
	// configured budget is visible.
	iterations := strconv.Itoa(m.Iteration)
	iterationsWidth := 4

	if m.MaxIterations > 0 {
		iterations = fmt.Sprintf("%d/%d", m.Iteration, m.MaxIterations)
		iterationsWidth = lipgloss.Width(fmt.Sprintf("%d/%d", m.MaxIterations, m.MaxIterations))
	}

	elapsed := FmtDuration(m.Elapsed)
	if m.MaxDuration > 0 {
		elapsed += "/" + FmtDuration(m.MaxDuration)
	}

	// Live values sit in fixed-width cells (see cell) so a number gaining a digit does not shove every later
	// segment sideways. A value that outgrows its cell renders whole, and the bar shifts once rather than clipping.
	segments := []string{
		seg("provider", m.provider, metaProvider),
		seg("model", m.model, metaModel),
		seg("iter", cell(iterations, iterationsWidth), metaCount),
		seg("elapsed", elapsed, metaStyle),
		seg("tokens", fmt.Sprintf("↑%s ↓%s", cell(FmtTokens(m.InputTokens), 6), cell(FmtTokens(m.OutputTokens), 6)), metaModel),
		seg("dir", ShortPath(m.Workdir, 28), metaStyle),
	}

	// A segment is shown whole or not at all, since clipping left half-rendered segments ("elap", "tok") that
	// read as a broken UI. The bar takes segments in order while they fit and stops at the first that does not.
	separator := metaStyle.Render("  ·  ")
	separatorWidth := lipgloss.Width(separator)

	parts := make([]string, 0, len(segments))
	used := 0

	for _, segment := range segments {
		needed := lipgloss.Width(segment)
		if len(parts) > 0 {
			needed += separatorWidth
		}

		// Width is zero until the first WindowSizeMsg arrives. There is no
		// terminal to fit yet, so nothing is dropped for not fitting it.
		if m.Width > 0 && used+needed > m.Width {
			break
		}

		parts = append(parts, segment)
		used += needed
	}

	line := strings.Join(parts, separator)

	return lipgloss.NewStyle().MaxWidth(m.Width).Render(line)
}

func (m *Model) Footer() string {
	hints := footerStyle.Render(
		keyHint.Render("↑/↓") + " scroll  " +
			keyHint.Render("g/G") + " top/bottom  " +
			keyHint.Render("q") + " quit",
	)
	if m.Status == StatusRunning {
		return hints
	}

	tail := footerStyle.Render("  ·  press " + keyHint.Render("q") + " to exit")

	return hints + tail
}

// --- view -------------------------------------------------------------------.

func (m *Model) View() string {
	if !m.Ready {
		return "starting zot…"
	}

	return strings.Join([]string{
		m.TitleBar(),
		m.MetaBar(),
		"",
		m.Viewport.View(),
		m.Footer(),
	}, "\n")
}
