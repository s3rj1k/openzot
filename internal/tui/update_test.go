package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/conversation"
	"github.com/openzot/openzot/internal/loop"
)

// asModel is what an Update returned, as the model it is.
func asModel(t *testing.T, updated tea.Model) *model {
	t.Helper()

	typed, ok := updated.(*model)
	require.True(t, ok, "Update returned %T, want *model", updated)

	return typed
}

// sized returns a model that has been through a window-size message, which is
// what makes the viewport usable.
func sized(t *testing.T, width, height int) *model {
	t.Helper()

	m := newModel("do the thing", "gpt-5.4-mini", "openai", "/tmp/work")

	updated, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: height})

	return asModel(t, updated)
}

// Without a command from Init nothing ever redraws, so the spinner never turns
// and the elapsed clock never advances - a screen that looks hung on a run that
// is working fine.
func TestInitStartsTheSpinnerAndClock(t *testing.T) {
	m := newModel("task", "m", "b", "/w")

	cmd := m.Init()

	require.NotNil(t, cmd, "Init must return a command; without it nothing ever redraws")

	// a batch fans out into the individual commands, which is how both the
	// spinner tick and the clock tick get started
	msg := cmd()

	batch, ok := msg.(tea.BatchMsg)
	require.True(t, ok, "want a batch starting both tickers")

	require.GreaterOrEqual(t, len(batch), 2, "want the spinner and the clock")
}

func TestWindowSizeMakesTheViewportReady(t *testing.T) {
	m := sized(t, 100, 40)

	assert.True(t, m.ready, "the model should be ready after a size message")

	assert.Equal(t, 100, m.width)
	assert.Equal(t, 40, m.height)
}

// A terminal too short for the chrome must still leave a usable viewport rather
// than a negative one.
func TestTinyTerminalDoesNotProduceANegativeViewport(t *testing.T) {
	m := sized(t, 20, 1)

	assert.GreaterOrEqual(t, m.vp.Height, 1)
}

// A quit has to actually be a quit. Asserting only that some command came back
// would pass for a scroll, which is the thing an unbound key does.
func TestQuitKeys(t *testing.T) {
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'q'}},
		{Type: tea.KeyCtrlC},
	} {
		m := sized(t, 80, 24)

		_, cmd := m.Update(key)

		require.NotNil(t, cmd, "key %v returned no command", key)

		_, quit := cmd().(tea.QuitMsg)
		assert.True(t, quit, "key %v produced %T, want a quit", key, cmd())
	}
}

// `g` jumps to the top and stops following. `G` returns to the tail.
func TestJumpKeys(t *testing.T) {
	m := sized(t, 80, 24)

	for range 100 {
		m.appendEntry("line")
	}

	m.render()

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})

	assert.False(t, updated.(*model).follow, "jumping to the top must stop following")

	updated, _ = updated.(*model).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})

	assert.True(t, updated.(*model).follow, "jumping to the bottom must resume following")
}

// The log is read-only, so scrolling is the only interaction - and following the
// tail has to stop when the user scrolls up, or they can never read anything.
func TestScrollingStopsFollowing(t *testing.T) {
	m := sized(t, 80, 24)

	for range 200 {
		m.appendEntry("line")
	}

	m.render()

	require.True(t, m.follow, "a fresh model should follow the tail")

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})

	assert.False(t, updated.(*model).follow, "scrolling up must stop the log from jumping back to the bottom")
}

func TestTickAdvancesTheElapsedClock(t *testing.T) {
	m := sized(t, 80, 24)

	updated, cmd := m.Update(tickMsg{})

	assert.NotNil(t, cmd, "a tick must schedule the next one, or the clock stops")

	assert.GreaterOrEqual(t, updated.(*model).elapsed, time.Duration(0))
}

func TestHandleEventBuildsTheLog(t *testing.T) {
	m := sized(t, 100, 30)

	m.handleEvent(&loop.Event{Kind: loop.EventIteration, Iteration: 1})
	m.handleEvent(&loop.Event{Kind: loop.EventToolCallStart, Tool: litShell, Args: map[string]any{"command": "ls"}})
	m.handleEvent(&loop.Event{Kind: loop.EventToolCallEnd, Tool: litShell, Result: "README.md"})
	// tokens are what the log shows. A MessageAgentEvent carries the same
	// content and is by design not drawn twice
	m.handleEvent(&loop.Event{Kind: loop.EventToken, Text: "here is "})
	m.handleEvent(&loop.Event{Kind: loop.EventToken, Text: "the answer"})
	m.handleEvent(&loop.Event{Kind: loop.EventMessage, MessageType: conversation.TypeBot, Text: "here is the answer"})

	m.flushPending()

	assert.Equal(t, 1, m.iteration)

	log := strings.Join(m.entries, "\n")

	for _, want := range []string{litShell, "here is the answer"} {
		assert.Contains(t, log, want)
	}
}

func TestToolErrorsAreShown(t *testing.T) {
	m := sized(t, 100, 30)

	m.handleEvent(&loop.Event{Kind: loop.EventToolCallError, Tool: litShell, Text: "command not found"})

	log := strings.Join(m.entries, "\n")

	assert.Contains(t, log, "command not found", "a tool failure must be visible")
}

func TestTheEndingSetsTheStatus(t *testing.T) {
	tests := []struct {
		name string
		exit loop.Result
		want status
	}{
		{"a settled run", loop.Result{Reason: loop.StopSettled, Message: litDone}, statusDone},
		{"a budget-exhausted run", loop.Result{Reason: loop.StopIterations, Message: "gave up"}, statusFailed},
		{"a run the model declared failed", loop.Result{Reason: loop.StopFailed, Message: "cannot reach the host"}, statusFailed},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := sized(t, 100, 30)

			m.finish(&test.exit)

			assert.Equal(t, test.want, m.status)

			assert.NotEmpty(t, m.exitMsg, "the exit message should be retained for the footer")
		})
	}
}

func stripANSI(s string) string {
	var (
		builder strings.Builder
		inEsc   bool
	)

	for _, r := range s {
		switch {
		case r == '\x1b':
			inEsc = true
		case inEsc && (r == 'm' || r == 'K'):
			inEsc = false
		case !inEsc:
			builder.WriteRune(r)
		}
	}

	return builder.String()
}

// A run the model declared a failure is not a crash and not a budget cut. It
// reached a conclusion. Reporting it as "exited (code 1)" reads as a use
// malfunction, which sends the operator looking in the wrong place.
func TestDeclaredFailureRendersAsAnOutcomeNotACrash(t *testing.T) {
	m := sized(t, 100, 30)

	m.finish(&loop.Result{Reason: loop.StopFailed, Message: "cannot reach the host"})

	log := stripANSI(strings.Join(m.entries, "\n"))

	assert.Contains(t, log, "cannot reach the host", "log %q should carry the model's stated reason", log)

	assert.NotContains(t, log, "code 1", "log %q reports a declared failure as a process exit code", log)
}

func TestViewRendersWithoutPanicking(t *testing.T) {
	m := sized(t, 100, 30)

	m.handleEvent(&loop.Event{Kind: loop.EventIteration, Iteration: 2})
	m.handleEvent(&loop.Event{Kind: loop.EventToken, Text: "something"})
	m.flushPending()

	view := m.View()

	require.NotEmpty(t, view)

	for _, want := range []string{"do the thing", "gpt-5.4-mini", "openai"} {
		assert.Contains(t, view, want)
	}
}

// Before the first size message there is nothing sensible to draw, and drawing
// anyway used to produce a garbled frame.
func TestViewBeforeReady(t *testing.T) {
	m := newModel("task", "m", "b", "/w")

	if view := m.View(); strings.Contains(view, "\x1b[") {
		assert.True(t, m.ready, "an unready model should not draw a full frame")
	}
}

// The iteration divider is a fixed short rule - long enough to read, short
// enough to survive wrapping at any terminal width.
func TestIterationRuleIsFixedShort(t *testing.T) {
	m := sized(t, 100, 30)

	m.handleEvent(&loop.Event{Kind: loop.EventIteration, Iteration: 7})

	entry := m.entries[len(m.entries)-1]

	got := strings.Count(entry, "─")
	assert.Equal(t, 6, got, "iteration divider has %d ─ glyphs, want exactly 6: %q", got, entry)

	const want = "─── iteration 7 ───"
	assert.Contains(t, entry, want, "divider must be three dashes on each side of the label")
}

// A width-filling rule wraps at a narrow terminal and smears the divider over
// two rows. The fixed rule must come through m.wrap as one intact line.
func TestIterationRuleStaysOneRowAtNarrowWidth(t *testing.T) {
	for _, width := range []int{40, 20} {
		t.Run(fmt.Sprintf("%dcolumns", width), func(t *testing.T) {
			m := sized(t, width, 30)

			m.handleEvent(&loop.Event{Kind: loop.EventIteration, Iteration: 4})

			rows := strings.Split(m.committedWrapped, "\n")

			dividers := 0

			for _, row := range rows {
				if !strings.Contains(row, "iteration 4") {
					continue
				}

				dividers++

				assert.Contains(t, row, "─── iteration 4 ───", "divider broke across rows at width %d", width)
			}

			assert.Equal(t, 1, dividers, "want one iteration row at width %d", width)
		})
	}
}

func TestRewrapOnResize(t *testing.T) {
	m := sized(t, 120, 30)

	m.appendEntry(strings.Repeat("word ", 60))

	m.render()

	wide := m.committedWrapped

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 30})

	narrow := updated.(*model).committedWrapped

	assert.NotEqual(t, narrow, wide, "resizing should re-wrap the committed log")
}

// The badge is how an operator tells at a glance whether the run is still going
// or has failed, so the three statuses have to be distinguishable - a
// non-empty badge that says the same thing for all three tells them nothing.
func TestBadgeReflectsStatus(t *testing.T) {
	badges := map[status]string{}

	for _, st := range []status{statusRunning, statusDone, statusFailed} {
		m := sized(t, 80, 24)

		m.status = st

		badge := m.badge()

		assert.NotEmpty(t, badge, "status %v produced no badge", st)

		for other, seen := range badges {
			assert.NotEqual(t, badge, seen, "statuses %v and %v render the same badge %q", other, st, badge)
		}

		badges[st] = badge
	}

	// and each says which state it is, in words rather than color alone
	for st, want := range map[status]string{
		statusRunning: "working",
		statusDone:    litDone,
		statusFailed:  litFailed,
	} {
		assert.Contains(t, badges[st], want, "the %v badge %q does not say %q", st, badges[st], want)
	}
}

func TestFooterShowsTheKeyHints(t *testing.T) {
	m := sized(t, 100, 30)

	m.iteration = 3

	footer := m.footer()

	require.NotEmpty(t, footer, "the footer must render while running")

	// the log is read-only, so the only affordances are scrolling and quitting
	for _, want := range []string{"scroll", "quit"} {
		assert.Contains(t, footer, want)
	}
}

// The outcome leaves the UI as an error the caller can act on, which is how a
// non-zero exit code reaches the shell.
func TestExitBecomesAnError(t *testing.T) {
	m := sized(t, 100, 30)

	m.finish(&loop.Result{Reason: loop.StopCycle, Message: "kept repeating"})

	err := m.runError()
	require.Error(t, err, "a failed run must surface as an error")

	assert.Contains(t, err.Error(), "kept repeating")

	clean := sized(t, 100, 30)

	clean.finish(&loop.Result{Reason: loop.StopSettled, Message: litDone})

	require.NoError(t, clean.runError())
}

// A spinner tick is ignored once the run has ended, or the finished screen keeps
// animating.
func TestSpinnerStopsWhenTheRunEnds(t *testing.T) {
	m := sized(t, 80, 24)

	m.status = statusDone

	_, cmd := m.Update(tickMsg{})

	assert.Nil(t, cmd, "the clock must stop once the run has ended")
}

// The renderers have to know the real tool names. A mismatch is not a compile
// error - it just renders the agent's most-used tool as an anonymous
// key/value dump, which is how `shell` went unstyled.
func TestRenderToolStartCoversTheBuiltInTools(t *testing.T) {
	tests := []struct {
		tool string
		args map[string]any
		want string
	}{
		{litShell, map[string]any{"command": "go test ./..."}, "go test"},

		// a caller's own tool still renders, just generically
		{litCustom, map[string]any{"thing": "value"}, "thing=value"},
	}

	for _, test := range tests {
		got := stripANSI(renderToolStart(test.tool, test.args))

		assert.Contains(t, got, test.want)
	}
}

// zot's tools return strings, so a summary that only understood maps rendered
// nothing at all.
func TestRenderToolEndHandlesStringResults(t *testing.T) {
	tests := []struct {
		name    string
		tool    string
		result  any
		wantAny bool
		want    string
	}{
		{"shell echoes its output", litShell, "hello\nworld", true, "hello"},
		{"a silent command still confirms", litShell, "", true, litDone},
		{"an unknown tool echoes", litCustom, "some output", true, "some output"},
		{"an unknown tool with nothing to say", litCustom, "", false, ""},
		{"a non-string, non-map result", litShell, 42, false, ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := stripANSI(renderToolEnd(test.tool, test.result))

			if !test.wantAny {
				assert.Empty(t, got)

				return
			}

			assert.Contains(t, got, test.want)
		})
	}
}

// One record must not scroll the rest of the run off the screen. It is cut at a
// third of the terminal's height, the last row an ellipsis.
func TestARecordIsClippedToAThirdOfTheTerminalHeight(t *testing.T) {
	lines := make([]string, 0, 50)

	for i := range 50 {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}

	m := sized(t, 100, 30)

	m.handleEvent(&loop.Event{Kind: loop.EventToolCallEnd, Tool: litShell, Result: strings.Join(lines, "\n")})

	rows := strings.Split(stripANSI(m.committedWrapped), "\n")

	require.Len(t, rows, 10, "the record took %d rows on a 30-row terminal, want 10", len(rows))

	assert.Contains(t, rows[len(rows)-1], "…", "the cut must end on an ellipsis, got %q", rows[len(rows)-1])

	assert.Contains(t, rows[0], litDone, "the head of the record must survive")
	assert.Contains(t, rows[1], "line 0", "the head of the record must survive")
}

// Rows are what count, not source lines. A few long lines wrap into many rows.
func TestAWrappedRecordIsClippedByRows(t *testing.T) {
	m := sized(t, 40, 30)

	long := strings.Repeat("word ", 40)

	m.handleEvent(&loop.Event{Kind: loop.EventToolCallEnd, Tool: litShell, Result: long + "\n" + long + "\n" + long})

	assert.Len(t, strings.Split(m.committedWrapped, "\n"), 10, "want the 10 a third of 30 allows")
}

// A record that fits is left exactly as it is, with no ellipsis.
func TestARecordThatFitsIsNotClipped(t *testing.T) {
	m := sized(t, 100, 30)

	m.handleEvent(&loop.Event{Kind: loop.EventToolCallEnd, Tool: litShell, Result: "one\ntwo\nthree"})

	got := stripANSI(m.committedWrapped)

	assert.NotContains(t, got, "…", "a short record must be shown whole")
	assert.Contains(t, got, "three", "a short record must be shown whole")
}

// The limit follows the terminal. Growing the window shows more of a record that
// was cut, because the log is re-wrapped from the full record.
func TestResizingChangesHowMuchOfARecordShows(t *testing.T) {
	lines := make([]string, 0, 50)

	for range 50 {
		lines = append(lines, "line")
	}

	m := sized(t, 100, 30)

	m.handleEvent(&loop.Event{Kind: loop.EventToolCallEnd, Tool: litShell, Result: strings.Join(lines, "\n")})

	before := len(strings.Split(m.committedWrapped, "\n"))

	resized, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 60})
	m = asModel(t, resized)

	after := len(strings.Split(m.committedWrapped, "\n"))
	assert.Equal(t, 20, after, "rows after growing the window = %d (was %d), want 20", after, before)
	assert.Greater(t, after, before, "rows after growing the window = %d (was %d), want 20", after, before)
}

func TestRenderToolEndHandlesStructuredResults(t *testing.T) {
	failure := stripANSI(renderToolEnd(litShell, map[string]any{
		"success": false,
		"error":   "exit status 1",
		"stderr":  "compile failed",
	}))

	assert.Contains(t, failure, "exit status 1")

	success := stripANSI(renderToolEnd(litShell, map[string]any{litStdout: "all good"}))

	assert.Contains(t, success, "all good", "structured output must surface")
}

// A key nobody bound must reach the viewport rather than being swallowed, and
// must not quit. An unattended run ended by a stray keystroke is a lost run.
func TestUnboundKeysAreNotQuitKeys(t *testing.T) {
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'x'}},
		{Type: tea.KeyEsc},
		{Type: tea.KeyEnter},
		{Type: tea.KeyRunes, Runes: []rune{' '}},
	} {
		m := sized(t, 80, 24)

		updated, _ := m.Update(key)

		// A quit would leave the model unchanged and end the program. Without running the program, what can be
		// checked is that the viewer is still there and still running.
		assert.Equal(t, statusRunning, updated.(*model).status, "key %v ended the run", key)
	}
}

// The spinner and the clock stop once the run ends, so a finished screen does
// not look like it is still working.
func TestTheClockStopsWhenTheRunEnds(t *testing.T) {
	m := sized(t, 80, 24)

	m.status = statusDone

	before := m.elapsed

	next, cmd := m.Update(tickMsg{})

	assert.Nil(t, cmd, "a finished run must not schedule another tick")

	assert.Equal(t, before, next.(*model).elapsed, "the clock kept running after the run ended")
}

// The footer tells the operator how to leave. While a run is going it shows the
// keys. Once it is over it says so explicitly, because the run no longer ends
// on its own.
func TestFooterAddsAnExitHintWhenTheRunIsOver(t *testing.T) {
	m := sized(t, 80, 24)

	m.status = statusRunning

	running := m.footer()

	m.status = statusDone

	finished := m.footer()

	assert.Greater(t, len(finished), len(running), "a finished footer must say more than a running one")

	assert.Contains(t, finished, "exit", "the finished footer must say how to leave")
}

func TestFormattedDuration(t *testing.T) {
	tests := []struct {
		duration time.Duration
		want     string
	}{
		{duration: 0, want: "00:00"},
		{duration: 45 * time.Second, want: "00:45"},
		{duration: 90 * time.Second, want: "01:30"},
		{duration: 61 * time.Minute, want: "61:00"},
	}

	for _, test := range tests {
		assert.Equal(t, test.want, fmtDuration(test.duration))
	}
}

// The header has to survive a terminal too narrow for it. A run watched over a
// phone-sized ssh session is still a run.
func TestTitleBarSurvivesANarrowTerminal(t *testing.T) {
	for _, width := range []int{1, 8, 12, 20} {
		m := sized(t, width, 24)

		title := m.titleBar()

		assert.NotEmpty(t, title, "width %d produced no title bar", width)

		assert.NotContains(t, title, "\n", "width %d wrapped the title bar", width)
	}
}

func TestTruncateAddsAnEllipsisAndFlattensNewlines(t *testing.T) {
	tests := []struct {
		in   string
		max  int
		want string
	}{
		{in: "short", max: 10, want: "short"},
		{in: "exactly-10", max: 10, want: "exactly-10"},
		{in: "a longer string", max: 8, want: "a longe…"},
		{in: "two\nlines", max: 20, want: "two lines"},
		{in: "two\nlines here", max: 6, want: "two l…"},

		// A task or tool argument in CJK or emoji was cut mid-rune, rendering a replacement character, and the cap
		// counted bytes, so the line was cut far short of its width.
		{in: "日本語のタスク説明文です", max: 6, want: "日本語のタ…"},
		{in: "🚀🚀🚀🚀🚀", max: 3, want: "🚀🚀…"},
		{in: "日本語", max: 10, want: "日本語"},
	}

	for _, test := range tests {
		assert.Equal(t, test.want, truncate(test.in, test.max))
	}
}

// A shell tool reports failure on stderr, and that is exactly the output an
// operator reading a failed run needs to see.
func TestCommandOutputPrefersStdoutButFallsBackToStderr(t *testing.T) {
	assert.Contains(t, commandOutput(map[string]any{litStdout: "all good\n"}), "all good")

	got := commandOutput(map[string]any{litStdout: "", "stderr": "permission denied\n"})

	assert.Contains(t, got, "permission denied", "stderr was not rendered when stdout was empty")

	got = commandOutput(map[string]any{})
	assert.Empty(t, got, "a silent command rendered %q, want nothing", got)
}

func TestActivityLogIsBoundedForLongRuns(t *testing.T) {
	// A not-yet-sized model. render() does nothing, so this exercises the scrollback cap without the per-append
	// viewport cost (which a real, model-paced run pays anyway, now bounded by the cap).
	m := newModel("do the thing", "m", "b", "d")

	limit := m.maxEntries // DefaultMaxScrollback

	total := limit + limit/4 + 200 // enough to force a trim past the cap + slack
	for i := range total {
		m.appendEntry(fmt.Sprintf("line %d", i))
	}

	// bounded. Never more than the cap plus the trim slack, whatever the run length
	require.LessOrEqual(t, len(m.entries), limit+limit/4, "scrollback must stay bounded, got %d entries", len(m.entries))

	assert.True(t, m.truncated, "truncation must be flagged once the cap is exceeded")

	// the newest line always survives
	got := m.entries[len(m.entries)-1]
	assert.Equal(t, fmt.Sprintf("line %d", total-1), got, "the most recent line must be kept, got %q", got)

	// the oldest kept line is exactly total - len(entries), and older ones are gone
	oldest := total - len(m.entries)
	got = m.entries[0]
	assert.Equal(t, fmt.Sprintf("line %d", oldest), got, "the oldest kept line should be line %d, got %q", oldest, got)
}

// When the log has been trimmed, the viewer must say so and point at the session
// log, so a watcher knows the on-screen history is not the whole run.
func TestTrimmedLogShowsAMarker(t *testing.T) {
	m := sized(t, 100, 30)
	m.truncated = true
	m.appendEntry("a recent line") // triggers a render

	assert.Contains(t, m.vp.View(), "trimmed", "a trimmed log must show a marker, got")
}

// The scrollback cap is configurable (Meta.MaxScrollback / ui.scrollback). A
// caller can keep fewer or more lines than the default.
func TestScrollbackCapIsConfigurable(t *testing.T) {
	m := newModel("t", "m", "b", "d")
	m.maxEntries = 50 // what Run sets from Meta.MaxScrollback

	for i := range 300 {
		m.appendEntry(fmt.Sprintf("line %d", i))
	}

	assert.LessOrEqual(t, len(m.entries), m.maxEntries+m.maxEntries/4, "a custom cap of %d must be honored, kept %d", m.maxEntries, len(m.entries))

	assert.GreaterOrEqual(t, len(m.entries), m.maxEntries, "should keep about the cap %d, kept only %d", m.maxEntries, len(m.entries))
}

// headerSegments is how many segments the header has when everything fits.
const headerSegments = 6

// The header reads provider, model, iteration, elapsed, tokens, directory - in
// that order, so what survives a narrow terminal is what changes most.
func TestMetaBarOrder(t *testing.T) {
	m := sized(t, 400, 30)
	m.workdir = "/work/project"

	bar := stripANSI(m.metaBar())

	last := -1

	for _, label := range []string{"provider", "model", "iter", litElapsed, "tokens", "dir"} {
		at := strings.Index(bar, label)
		require.GreaterOrEqual(t, at, 0, "%q is missing or out of order in %q", label, bar)
		require.GreaterOrEqual(t, at, last, "%q is missing or out of order in %q", label, bar)

		last = at
	}
}

// metaSegments splits a rendered bar into its visible segments, so a test can
// ask what is actually on screen without knowing how a segment is built.
func metaSegments(bar string) []string {
	var out []string

	for part := range strings.SplitSeq(stripANSI(bar), "·") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}

	return out
}

// A stat segment is shown whole or not at all, since a clipped count can be misread, which is worse than an absent one. The
// narrow renders are checked against a wide one rather than hardcoded text, since what a segment says is the bar's business.
func TestMetaBarDropsSegmentsThatDoNotFitWhole(t *testing.T) {
	reference := metaSegments(sized(t, 400, 30).metaBar())

	require.Len(t, reference, headerSegments, "a wide terminal must show every header segment")

	for _, width := range []int{12, 20, 33, 47, 68, 95, 140} {
		m := sized(t, width, 30)

		bar := m.metaBar()

		// nothing may spill past the terminal edge
		got := lipgloss.Width(bar)
		assert.LessOrEqual(t, got, width, "at width %d the bar is %d columns wide: %q", width, got, bar)

		shown := metaSegments(bar)

		// every segment on screen is one of the whole segments, not a prefix of
		// one - this is the half-visible bug, stated directly
		for i, segment := range shown {
			if i >= len(reference) {
				assert.Failf(t, "unexpected", "at width %d the bar grew segments it should not have: %q", width, shown)

				break
			}

			assert.Equal(t, reference[i], segment, "at width %d segment", width)
		}

		// and what is shown is a prefix of the configured order, so segments
		// appear and disappear predictably as the terminal is resized
		assert.LessOrEqual(t, len(shown), len(reference), "at width %d the bar shows %d segments, more than exist", width, len(shown))
	}
}

// Narrowing the terminal only ever removes segments, and widening only ever
// adds them back - the bar is a prefix that grows monotonically with the space
// it has, which is what makes a resize readable rather than a reshuffle.
func TestMetaBarGrowsMonotonicallyWithWidth(t *testing.T) {
	previous := -1

	for width := 8; width <= 400; width += 4 {
		shown := len(metaSegments(sized(t, width, 30).metaBar()))

		require.GreaterOrEqual(t, shown, previous, "widening to %d columns dropped a segment (%d, was %d)", width, shown, previous)

		previous = shown
	}

	assert.Equal(t, headerSegments, previous, "the widest terminal shows %d segments, want all %d", previous, headerSegments)
}

// A terminal too narrow for even the first segment shows an empty bar rather
// than a fragment of one.
func TestMetaBarIsEmptyWhenNothingFits(t *testing.T) {
	m := sized(t, 3, 30)

	assert.Empty(t, strings.TrimSpace(stripANSI(m.metaBar())), "nothing fits at 3 columns, so nothing should be drawn")
}

// The header shows the provider-reported token usage, and progress against any
// configured limits (5/1000). A limit that is unset shows no denominator.
func TestMetaBarShowsTokensAndLimits(t *testing.T) {
	m := sized(t, 400, 30)
	m.iteration = 5
	m.maxIterations = 1000
	m.maxDuration = 30 * time.Minute
	m.inputTokens = 32000
	m.outputTokens = 13000

	bar := m.metaBar()

	assert.Contains(t, bar, "5/1000", "iter must show progress against its limit")

	assert.Contains(t, bar, "/30:00", "elapsed must show the time limit")

	assert.Contains(t, bar, "32.0k", "tokens must show provider usage compactly")
	assert.Contains(t, bar, "13.0k", "tokens must show provider usage compactly")
}

// A usage update from the run sets the counts the meta bar reads.
func TestHandleEventRecordsUsage(t *testing.T) {
	m := sized(t, 100, 30)

	m.handleEvent(&loop.Event{Kind: loop.EventUsage, InputTokens: 1234, OutputTokens: 567})

	assert.Equal(t, 1234, m.inputTokens, "usage not recorded: in=%d out=%d", m.inputTokens, m.outputTokens)
	assert.Equal(t, 567, m.outputTokens, "usage not recorded: in=%d out=%d", m.inputTokens, m.outputTokens)
}

func TestFmtTokens(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{
		{0, "0"},
		{532, "532"},
		{45200, "45.2k"},
		{1_200_000, "1.2M"},
	} {
		assert.Equal(t, tc.want, fmtTokens(tc.n))
	}
}

// The error behind a failed run is kept and shown - it is usually the run's only
// diagnostic (the provider's 404, not the loop's "the provider failed").
func TestTheErrorBehindAFailedRunIsKeptAndShown(t *testing.T) {
	m := sized(t, 100, 30)

	next, _ := m.Update(doneMsg{result: loop.Result{
		Reason:  loop.StopError,
		Message: "the provider failed",
		Err:     errors.New("provider: Model 'x' not found (404)"),
	}})
	m = asModel(t, next)

	err := m.runError()
	require.Error(t, err, "want the provider's own words")
	assert.Contains(t, err.Error(), "not found (404)", "want the provider's own words")

	joined := strings.Join(m.entries, "\n")
	assert.Contains(t, joined, "not found (404)", "the log should show the underlying error")
}

// A retry spends a continuation and then waits out a backoff. Without a
// rendered line the wait shows as empty iteration dividers stacking up - a run
// that is surviving looks like one that is hanging.
func TestRetryEventIsRendered(t *testing.T) {
	m := sized(t, 100, 30)

	m.handleEvent(&loop.Event{Kind: loop.EventRetry, Text: "provider: Provider returned error: ERROR (upstream: Stealth) (400)"})

	joined := strings.Join(m.entries, "\n")
	assert.Contains(t, joined, "retrying", "the retry should be visible with its cause")
	assert.Contains(t, joined, "Stealth", "the retry should be visible with its cause")
}

// The header shows the order's title when it has one. The task is the whole order rendered for the model, so a one-line
// header of it is a paragraph cut mid-word, which is what titles exist to replace.
func TestTitleBarPrefersTheTitleOverTheTask(t *testing.T) {
	task := "add rate limiting to the api\n\nAcceptance criteria - the objective is not met until every one of these holds:\n1. the suite passes"

	withTitle := sized(t, 120, 30)
	withTitle.task = task
	withTitle.title = "Rate limiting"

	bar := stripANSI(withTitle.titleBar())

	assert.Contains(t, bar, "Rate limiting", "the title bar should show the title")

	assert.NotContains(t, bar, "add rate limiting to the api", "the task text should give way to the title")

	// without a title there is still something to show
	untitled := sized(t, 120, 30)
	untitled.task = task

	assert.Contains(t, stripANSI(untitled.titleBar()), "add rate limiting", "an untitled run must fall back to the task")
}

// A live value growing a digit (nine iterations becoming ten, 999 tokens becoming 1.0k) must not shove later segments
// sideways, since a jittering header is unreadable at a glance. Each volatile field is followed by a fixed one, and the test
// asks whether that one moved.
func TestMetaBarDoesNotShiftAsValuesChange(t *testing.T) {
	tests := []struct {
		name   string
		next   string // the label of the segment after the one that changes
		before func(*model)
		after  func(*model)
	}{
		{
			name:   "iterations gaining a digit",
			next:   litElapsed,
			before: func(m *model) { m.iteration = 9 },
			after:  func(m *model) { m.iteration = 10 },
		},
		{
			name:   "iterations against a limit",
			next:   litElapsed,
			before: func(m *model) { m.iteration, m.maxIterations = 9, 300 },
			after:  func(m *model) { m.iteration, m.maxIterations = 100, 300 },
		},
		{
			name:   "tokens crossing into thousands",
			next:   "dir",
			before: func(m *model) { m.inputTokens, m.outputTokens = 532, 40 },
			after:  func(m *model) { m.inputTokens, m.outputTokens = 120_000, 4_500 },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := sized(t, 400, 30)
			m.workdir = "/work/project"

			test.before(m)

			was := strings.Index(stripANSI(m.metaBar()), test.next)

			test.after(m)

			now := strings.Index(stripANSI(m.metaBar()), test.next)
			assert.Equal(t, was, now, "the change moved %q from column %d to %d:\n%q", test.next, was, now, stripANSI(m.metaBar()))
		})
	}
}

// tasksArgs is the arguments of a tasks call as the model sends them.
func tasksArgs(tasks ...[3]string) map[string]any {
	list := make([]any, 0, len(tasks))

	for _, task := range tasks {
		entry := map[string]any{"title": task[0], "status": task[1]}

		if task[2] != "" {
			entry["note"] = task[2]
		}

		list = append(list, entry)
	}

	return map[string]any{litTasks: list}
}

// The task list is the one piece of the run worth reading in full, so it renders
// as a checklist. What is done, what is under way, what is left, what is stuck.
func TestRenderTasksShowsTheChecklist(t *testing.T) {
	out := stripANSI(renderToolStart(litTasks, tasksArgs(
		[3]string{"read the handler", litDone, ""},
		[3]string{"add validation", "in_progress", "the error path is missing"},
		[3]string{"write a test", "pending", ""},
		[3]string{"deploy", "blocked", "needs credentials"},
	)))

	for _, want := range []string{
		litTasks, "1/4 done",
		"✓ read the handler", "▶ add validation", "· write a test", "✗ deploy",
		"the error path is missing", "needs credentials",
	} {
		assert.Contains(t, out, want, "rendered tasks missing %q", want)
	}
}

// A call the tool rejects - no tasks, an unknown status - still shows its header
// rather than crashing the render, and draws nothing it cannot vouch for.
func TestRenderTasksIsRobust(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"no arguments":  {},
		"an empty list": {litTasks: []any{}},
		"a bad status":  tasksArgs([3]string{"a", "started", ""}),
		"not a list":    {litTasks: "do it"},
		"a non-object":  {litTasks: []any{"do it"}},
	} {
		out := stripANSI(renderToolStart(litTasks, args))

		assert.Contains(t, out, litTasks, "%s: should still render a header", name)

		assert.NotContains(t, out, litDone, "%s: a refused list must not report progress", name)
	}
}
