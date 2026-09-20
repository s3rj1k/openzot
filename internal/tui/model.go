package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/openzot/openzot/internal/loop"
)

type status int

const (
	statusRunning status = iota
	statusDone
	statusFailed
)

// reserved counts the non-viewport rows: title + meta + a blank gap + footer.
const reserved = 4

// tickMsg drives the elapsed-time clock once a second while the agent runs.
type tickMsg struct{}

// model is the entire read-only UI. It holds no input field by design: the user
// watches, they do not type. Everything it shows is derived from the agent's
// event stream plus a couple of counters.
type model struct {
	task     string
	title    string // shown instead of task when set - see tui.Meta.Title
	model    string
	provider string
	workdir  string

	spinner spinner.Model
	vp      viewport.Model
	ready   bool
	width   int
	height  int

	// Activity log. entries are the committed, logical lines; committedWrapped
	// caches them word-wrapped to the current width so per-token redraws stay
	// cheap. pending holds the assistant's in-flight narration.
	entries          []string
	committedWrapped string
	pending          string
	follow           bool // auto-scroll to the newest activity
	truncated        bool // oldest lines have been dropped to bound memory
	maxEntries       int  // scrollback cap (DefaultMaxScrollback unless overridden)

	status     status
	iteration  int
	exitCode   int
	exitReason string
	exitMsg    string
	err        error

	// Provider-reported cumulative token usage (not a local estimate).
	inputTokens  int
	outputTokens int

	// Configured limits, for the "5/1000" progress display. Zero means the limit
	// is unbounded (or the caller chose not to show it), so no denominator shows.
	maxIterations int
	maxDuration   time.Duration

	startedAt time.Time
	elapsed   time.Duration
}

func newModel(task, modelName, provider, workdir string) model {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(colYellow)

	return model{
		task:       task,
		model:      modelName,
		provider:   provider,
		workdir:    workdir,
		spinner:    sp,
		status:     statusRunning,
		follow:     true,
		startedAt:  time.Now(),
		maxEntries: DefaultMaxScrollback,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, tickCmd())
}

func tickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		vpHeight := max(msg.Height-reserved, 1)
		if !m.ready {
			m.vp = viewport.New(msg.Width, vpHeight)
			m.ready = true
		} else {
			m.vp.Width = msg.Width
			m.vp.Height = vpHeight
		}
		m.rewrap()
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "g", "home":
			m.vp.GotoTop()
			m.follow = false
			return m, nil
		case "G", "end":
			m.vp.GotoBottom()
			m.follow = true
			return m, nil
		}
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		m.follow = m.vp.AtBottom()
		return m, cmd

	case spinner.TickMsg:
		if m.status != statusRunning {
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case tickMsg:
		if m.status != statusRunning {
			return m, nil
		}
		m.elapsed = time.Since(m.startedAt)
		return m, tickCmd()

	case eventMsg:
		m.handleEvent(msg.ev)
		return m, nil

	case doneMsg:
		m.finish(msg.result)
		return m, nil
	}

	return m, nil
}

// handleEvent folds one event of the run into the UI state.
func (m *model) handleEvent(ev loop.Event) {
	switch ev.Kind {
	case loop.EventIteration:
		m.iteration = ev.Iteration
		m.flushPending()
		// A fixed short rule: one that fills the width would wrap at a narrow
		// terminal and smear the divider across two rows.
		m.appendEntry(dividerStyle.Render(fmt.Sprintf("─── iteration %d ───", ev.Iteration)))

	case loop.EventToken:
		m.pending += ev.Text
		m.render()

	case loop.EventToolCallStart:
		m.flushPending()
		m.appendEntry(renderToolStart(ev.Tool, ev.Args))

	case loop.EventToolCallEnd:
		if s := renderToolEnd(ev.Tool, ev.Result); s != "" {
			m.appendEntry(s)
		}

	case loop.EventToolCallError:
		m.appendEntry(errStyle.Render("    ✗ " + ev.Tool + ": " + ev.Text))

	case loop.EventNotice:
		// a corrective nudge - an empty turn, a truncation continuation, a
		// settle reminder; without this line the recovery renders as bare
		// iteration dividers, indistinguishable from a hang
		m.flushPending()
		m.appendEntry(statusRunningStyle.Render("⚠ ") + metaStyle.Render(ev.Text))

	case loop.EventRetry:
		// a retried provider failure spends a continuation and then waits out a
		// backoff; without this line the wait renders as empty iterations
		// stacking up - a run that is surviving looks like one that is hanging
		m.flushPending()
		m.appendEntry(statusRunningStyle.Render("↻ retrying") + "  " + metaStyle.Render(ev.Text))

	case loop.EventUsage:
		// provider-reported cumulative token usage, shown in the meta bar
		m.inputTokens = ev.InputTokens
		m.outputTokens = ev.OutputTokens
	}
}

// finish folds the run's ending into the UI state: the verdict, and the error
// behind it when there is one. The error is usually the run's only diagnostic -
// "the provider failed" on screen with the actual 404 dropped on the floor was
// how that got lost - so it is kept and shown.
func (m *model) finish(result loop.Result) {
	code := result.ExitCode()

	m.exitCode = code
	m.exitReason = string(result.Reason)
	m.exitMsg = result.Message
	m.flushPending()

	switch {
	case code == 0:
		m.status = statusDone
		m.appendEntry("\n" + okStyle.Render("✓ done") + "  " + taskStyle.Render(result.Message))

	case result.Reason == loop.StopFailed:
		// the model reached a conclusion and the conclusion is "no" - an
		// outcome, not a malfunction, so it does not get a process exit code
		m.status = statusFailed
		m.appendEntry("\n" + errStyle.Render("✗ failed") + "  " + taskStyle.Render(result.Message))

	default:
		m.status = statusFailed
		m.appendEntry("\n" + errStyle.Render(fmt.Sprintf("✗ exited (code %d)", code)) + "  " + taskStyle.Render(result.Message))
	}

	if result.Err != nil && m.err == nil {
		m.err = result.Err
		m.appendEntry(errStyle.Render("✗ " + result.Err.Error()))
	}
}

// --- viewport content management --------------------------------------------

// DefaultMaxScrollback is the on-screen log cap used when a caller does not set
// its own (Meta.MaxScrollback). An autonomous run can emit an unbounded number of
// events (millions of iterations, unbounded tool calls), so keeping every line
// would grow the viewer's memory without limit; once the cap is reached the
// oldest lines are dropped, and the full untrimmed run is always in the session
// log on disk. A caller that wants to keep more on screen raises the cap.
const DefaultMaxScrollback = 5000

func (m *model) appendEntry(s string) {
	m.entries = append(m.entries, s)

	// The buffer may grow a quarter past the cap before trimming, so the (linear)
	// re-wrap a trim costs is amortised over many appends rather than paid on every
	// append once the cap is reached.
	slack := m.maxEntries / 4

	if len(m.entries) > m.maxEntries+slack {
		// Keep the most recent m.maxEntries, copied into a fresh slice so the old
		// backing array is released rather than retained behind a reslice, then
		// rebuild the wrapped cache from the trimmed set.
		kept := make([]string, m.maxEntries)
		copy(kept, m.entries[len(m.entries)-m.maxEntries:])
		m.entries = kept
		m.truncated = true
		m.rewrap()

		return
	}

	// Append only the newly wrapped entry to the cache. Wrapping each entry to the
	// viewport width is equivalent to wrapping the whole joined buffer - the wrap
	// is per line - so per-append cost stays independent of how long the run has
	// been, instead of re-wrapping the entire history on every line.
	wrapped := m.wrapRecord(s)
	if m.committedWrapped == "" {
		m.committedWrapped = wrapped
	} else {
		m.committedWrapped += "\n" + wrapped
	}

	m.render()
}

// flushPending commits any streamed assistant narration as a dim thought block.
func (m *model) flushPending() {
	text := strings.TrimSpace(m.pending)
	m.pending = ""
	if text == "" {
		return
	}
	m.appendEntry(thoughtStyle.Render("  ◆ " + text))
}

// rewrap recomputes the cached content for a new width.
func (m *model) rewrap() {
	wrapped := make([]string, len(m.entries))
	for i, entry := range m.entries {
		wrapped[i] = m.wrapRecord(entry)
	}

	m.committedWrapped = strings.Join(wrapped, "\n")
	m.render()
}

// render pushes the current committed log plus any in-flight narration into the
// viewport, keeping the latest activity in view when following.
func (m *model) render() {
	if !m.ready {
		return
	}
	body := m.committedWrapped
	if m.truncated {
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
	m.vp.SetContent(body)
	if m.follow {
		m.vp.GotoBottom()
	}
}

func (m *model) wrap(s string) string {
	if s == "" || m.vp.Width <= 0 {
		return s
	}
	return lipgloss.NewStyle().Width(m.vp.Width).Render(s)
}

// recordHeight is the most rows one log record may take: a third of the
// terminal's height, so no single record - a chatty command, a long task list, a
// wall of narration - can push the rest of the run off the screen. Zero, meaning
// no limit, until the terminal has reported a size.
func (m model) recordHeight() int {
	if m.height <= 0 {
		return 0
	}

	return max(m.height/3, 2)
}

// wrapRecord wraps one log record to the viewport width and cuts it at
// recordHeight rows, the last of which is an ellipsis when anything was dropped.
// The full run is always in the session log.
func (m model) wrapRecord(s string) string {
	limit := m.recordHeight()
	if limit == 0 {
		return m.wrap(s)
	}

	// Every source line takes at least one row, so lines past the limit can never
	// be shown; dropping them first keeps a huge output from being wrapped whole.
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

// --- view -------------------------------------------------------------------

func (m model) View() string {
	if !m.ready {
		return "starting zot…"
	}
	return strings.Join([]string{
		m.titleBar(),
		m.metaBar(),
		"",
		m.vp.View(),
		m.footer(),
	}, "\n")
}

func (m model) titleBar() string {
	left := titleStyle.Render("✦ zot") + " " + m.badge()
	room := m.width - lipgloss.Width(left) - 2
	if room < 8 {
		return left
	}
	// A title is what the header wants: the task is the whole order rendered
	// for the model, so a one-line header of it is a paragraph cut mid-word.
	label := m.title
	if label == "" {
		label = m.task
	}

	return left + " " + taskStyle.Render(truncate(label, room))
}

func (m model) badge() string {
	switch m.status {
	case statusDone:
		return statusDoneStyle.Render("✓ done")
	case statusFailed:
		return statusFailStyle.Render("✗ failed")
	default:
		// Keep the spinner and label as separate same-colour pieces: nesting the
		// spinner's own ANSI inside another style breaks the run of colour.
		return m.spinner.View() + statusRunningStyle.Render("working")
	}
}

// metaBar is the header: provider, model, iteration, elapsed time, tokens and
// directory, in that order.
//
// The order is load-bearing. The bar is one line and drops what does not fit, so
// what comes first is what survives a narrow terminal. "dir" is last despite
// being useful because it never changes: a static path is not worth the live
// numbers it would push off the end.
func (m model) metaBar() string {
	seg := func(k, v string, value lipgloss.Style) string {
		return metaKey.Render(k+" ") + value.Render(v)
	}

	// iterations renders "n" or "n/max" when a limit is set, so progress against a
	// configured budget is visible.
	iterations := fmt.Sprintf("%d", m.iteration)
	iterationsWidth := 4

	if m.maxIterations > 0 {
		iterations = fmt.Sprintf("%d/%d", m.iteration, m.maxIterations)
		iterationsWidth = lipgloss.Width(fmt.Sprintf("%d/%d", m.maxIterations, m.maxIterations))
	}

	elapsed := fmtDuration(m.elapsed)
	if m.maxDuration > 0 {
		elapsed += "/" + fmtDuration(m.maxDuration)
	}

	// Live values sit in fixed-width cells (see cell) so a number growing a digit
	// - 9 to 10 iterations, 999 to 1.0k tokens - does not shove every segment
	// after it sideways. The cell widths are the widest value each field normally
	// shows; a value that outgrows its cell still renders whole, and the bar shifts
	// once rather than clipping.
	segments := []string{
		seg("provider", m.provider, metaProvider),
		seg("model", m.model, metaModel),
		seg("iter", cell(iterations, iterationsWidth), metaCount),
		seg("elapsed", elapsed, metaStyle),
		seg("tokens", fmt.Sprintf("↑%s ↓%s", cell(fmtTokens(m.inputTokens), 6), cell(fmtTokens(m.outputTokens), 6)), metaModel),
		seg("dir", shortPath(m.workdir, 28), metaStyle),
	}

	// A segment is shown whole or not at all. Clipping the line to the terminal
	// width left whichever segment straddled the edge half-rendered - "elap",
	// "tok" - which reads as a broken UI rather than a narrow one, and a
	// half-written number is worse than no number: it can be misread. So the
	// bar takes segments in order for as long as they fit and stops at the first
	// that does not, giving a prefix that grows and shrinks predictably as the
	// terminal is resized.
	separator := metaStyle.Render("  ·  ")
	separatorWidth := lipgloss.Width(separator)

	parts := make([]string, 0, len(segments))
	used := 0

	for _, segment := range segments {
		needed := lipgloss.Width(segment)
		if len(parts) > 0 {
			needed += separatorWidth
		}

		// width is zero until the first WindowSizeMsg arrives; there is no
		// terminal to fit yet, so nothing is dropped for not fitting it.
		if m.width > 0 && used+needed > m.width {
			break
		}

		parts = append(parts, segment)
		used += needed
	}

	line := strings.Join(parts, separator)

	return lipgloss.NewStyle().MaxWidth(m.width).Render(line)
}

func (m model) footer() string {
	hints := footerStyle.Render(
		keyHint.Render("↑/↓") + " scroll  " +
			keyHint.Render("g/G") + " top/bottom  " +
			keyHint.Render("q") + " quit",
	)
	if m.status == statusRunning {
		return hints
	}
	tail := footerStyle.Render("  ·  press " + keyHint.Render("q") + " to exit")
	return hints + tail
}

// cell pads v on the right to at least width columns, so a value that changes
// length - a count gaining a digit, a rate dropping its decimal - keeps the
// segments after it where they were. The value stays flush against its label;
// the slack trails. A value wider than the cell is returned whole.
func cell(v string, width int) string {
	if pad := width - lipgloss.Width(v); pad > 0 {
		return v + strings.Repeat(" ", pad)
	}

	return v
}

// fmtTokens renders a token count compactly: 532, 45.2k, 1.2M.
func fmtTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	return fmt.Sprintf("%02d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}
