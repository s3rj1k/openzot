package tui

import (
	"fmt"
	"github.com/charmbracelet/lipgloss"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/openzot/openzot/internal/conversation"
	"github.com/openzot/openzot/internal/loop"
)

// sized returns a model that has been through a window-size message, which is
// what makes the viewport usable.
func sized(t *testing.T, width, height int) model {
	t.Helper()

	m := newModel("do the thing", "gpt-5.4-mini", "openai", "/tmp/work")

	updated, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: height})

	typed, ok := updated.(model)
	if !ok {
		t.Fatalf("Update returned %T, want model", updated)
	}

	return typed
}

// Without a command from Init nothing ever redraws, so the spinner never turns
// and the elapsed clock never advances - a screen that looks hung on a run that
// is working fine.
func TestInitStartsTheSpinnerAndClock(t *testing.T) {
	m := newModel("task", "m", "b", "/w")

	cmd := m.Init()

	if cmd == nil {
		t.Fatal("Init must return a command; without it nothing ever redraws")
	}

	// a batch fans out into the individual commands, which is how both the
	// spinner tick and the clock tick get started
	msg := cmd()

	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		t.Fatalf("Init produced %T, want a batch starting both tickers", msg)
	}

	if len(batch) < 2 {
		t.Fatalf("Init started %d commands, want the spinner and the clock", len(batch))
	}
}

func TestWindowSizeMakesTheViewportReady(t *testing.T) {
	m := sized(t, 100, 40)

	if !m.ready {
		t.Error("the model should be ready after a size message")
	}

	if m.width != 100 || m.height != 40 {
		t.Errorf("size = %dx%d, want 100x40", m.width, m.height)
	}
}

// A terminal too short for the chrome must still leave a usable viewport rather
// than a negative one.
func TestTinyTerminalDoesNotProduceANegativeViewport(t *testing.T) {
	m := sized(t, 20, 1)

	if m.vp.Height < 1 {
		t.Errorf("viewport height = %d, want at least 1", m.vp.Height)
	}
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

		if cmd == nil {
			t.Fatalf("key %v returned no command", key)
		}

		if _, quit := cmd().(tea.QuitMsg); !quit {
			t.Errorf("key %v produced %T, want a quit", key, cmd())
		}
	}
}

// `g` jumps to the top and stops following; `G` returns to the tail.
func TestJumpKeys(t *testing.T) {
	m := sized(t, 80, 24)

	for i := 0; i < 100; i++ {
		m.appendEntry("line")
	}

	m.render()

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})

	if updated.(model).follow {
		t.Error("jumping to the top must stop following")
	}

	updated, _ = updated.(model).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})

	if !updated.(model).follow {
		t.Error("jumping to the bottom must resume following")
	}
}

// The log is read-only, so scrolling is the only interaction - and following the
// tail has to stop when the user scrolls up, or they can never read anything.
func TestScrollingStopsFollowing(t *testing.T) {
	m := sized(t, 80, 24)

	for i := 0; i < 200; i++ {
		m.appendEntry("line")
	}

	m.render()

	if !m.follow {
		t.Fatal("a fresh model should follow the tail")
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyUp})

	if updated.(model).follow {
		t.Error("scrolling up must stop the log from jumping back to the bottom")
	}
}

func TestTickAdvancesTheElapsedClock(t *testing.T) {
	m := sized(t, 80, 24)

	updated, cmd := m.Update(tickMsg{})

	if cmd == nil {
		t.Error("a tick must schedule the next one, or the clock stops")
	}

	if updated.(model).elapsed < 0 {
		t.Error("elapsed time must not be negative")
	}
}

func TestHandleEventBuildsTheLog(t *testing.T) {
	m := sized(t, 100, 30)

	m.handleEvent(loop.Event{Kind: loop.EventIteration, Iteration: 1})
	m.handleEvent(loop.Event{Kind: loop.EventToolCallStart, Tool: "shell", Args: map[string]any{"command": "ls"}})
	m.handleEvent(loop.Event{Kind: loop.EventToolCallEnd, Tool: "shell", Result: "README.md"})
	// tokens are what the log shows; a MessageAgentEvent carries the same
	// content and is deliberately not drawn twice
	m.handleEvent(loop.Event{Kind: loop.EventToken, Text: "here is "})
	m.handleEvent(loop.Event{Kind: loop.EventToken, Text: "the answer"})
	m.handleEvent(loop.Event{Kind: loop.EventMessage, MessageType: conversation.TypeBot, Text: "here is the answer"})

	m.flushPending()

	if m.iteration != 1 {
		t.Errorf("iteration = %d, want 1", m.iteration)
	}

	log := strings.Join(m.entries, "\n")

	for _, want := range []string{"shell", "here is the answer"} {
		if !strings.Contains(log, want) {
			t.Errorf("log is missing %q:\n%s", want, log)
		}
	}
}

func TestToolErrorsAreShown(t *testing.T) {
	m := sized(t, 100, 30)

	m.handleEvent(loop.Event{Kind: loop.EventToolCallError, Tool: "shell", Text: "command not found"})

	log := strings.Join(m.entries, "\n")

	if !strings.Contains(log, "command not found") {
		t.Errorf("a tool failure must be visible:\n%s", log)
	}
}

func TestTheEndingSetsTheStatus(t *testing.T) {
	tests := []struct {
		name string
		exit loop.Result
		want status
	}{
		{"a settled run", loop.Result{Reason: loop.StopSettled, Message: "done"}, statusDone},
		{"a budget-exhausted run", loop.Result{Reason: loop.StopIterations, Message: "gave up"}, statusFailed},
		{"a run the model declared failed", loop.Result{Reason: loop.StopFailed, Message: "cannot reach the host"}, statusFailed},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m := sized(t, 100, 30)

			m.finish(test.exit)

			if m.status != test.want {
				t.Errorf("status = %v, want %v", m.status, test.want)
			}

			if m.exitMsg == "" {
				t.Error("the exit message should be retained for the footer")
			}
		})
	}
}

// A run the model declared a failure is not a crash and not a budget cut: it
// reached a conclusion. Reporting it as "exited (code 1)" reads as a harness
// malfunction, which sends the operator looking in the wrong place.
func TestDeclaredFailureRendersAsAnOutcomeNotACrash(t *testing.T) {
	m := sized(t, 100, 30)

	m.finish(loop.Result{Reason: loop.StopFailed, Message: "cannot reach the host"})

	log := stripANSI(strings.Join(m.entries, "\n"))

	if !strings.Contains(log, "cannot reach the host") {
		t.Errorf("log %q should carry the model's stated reason", log)
	}

	if strings.Contains(log, "code 1") {
		t.Errorf("log %q reports a declared failure as a process exit code", log)
	}
}

func TestViewRendersWithoutPanicking(t *testing.T) {
	m := sized(t, 100, 30)

	m.handleEvent(loop.Event{Kind: loop.EventIteration, Iteration: 2})
	m.handleEvent(loop.Event{Kind: loop.EventToken, Text: "something"})
	m.flushPending()

	view := m.View()

	if view == "" {
		t.Fatal("View produced nothing")
	}

	for _, want := range []string{"do the thing", "gpt-5.4-mini", "openai"} {
		if !strings.Contains(view, want) {
			t.Errorf("view is missing %q", want)
		}
	}
}

// Before the first size message there is nothing sensible to draw, and drawing
// anyway used to produce a garbled frame.
func TestViewBeforeReady(t *testing.T) {
	m := newModel("task", "m", "b", "/w")

	if view := m.View(); strings.Contains(view, "\x1b[") && m.ready {
		t.Error("an unready model should not draw a full frame")
	}
}

// The iteration divider is a fixed short rule - long enough to read, short
// enough to survive wrapping at any terminal width.
func TestIterationRuleIsFixedShort(t *testing.T) {
	m := sized(t, 100, 30)

	m.handleEvent(loop.Event{Kind: loop.EventIteration, Iteration: 7})

	entry := m.entries[len(m.entries)-1]

	if got := strings.Count(entry, "─"); got != 6 {
		t.Errorf("iteration divider has %d ─ glyphs, want exactly 6: %q", got, entry)
	}

	const want = "─── iteration 7 ───"
	if !strings.Contains(entry, want) {
		t.Errorf("divider must be three dashes on each side of the label: %q", entry)
	}
}

// A width-filling rule wraps at a narrow terminal and smears the divider over
// two rows; the fixed rule must come through m.wrap as one intact line.
func TestIterationRuleStaysOneRowAtNarrowWidth(t *testing.T) {
	for _, width := range []int{40, 20} {
		t.Run(fmt.Sprintf("%dcolumns", width), func(t *testing.T) {
			m := sized(t, width, 30)

			m.handleEvent(loop.Event{Kind: loop.EventIteration, Iteration: 4})

			rows := strings.Split(m.committedWrapped, "\n")

			dividers := 0
			for _, row := range rows {
				if !strings.Contains(row, "iteration 4") {
					continue
				}
				dividers++
				if !strings.Contains(row, "─── iteration 4 ───") {
					t.Errorf("divider broke across rows at width %d: %q", width, row)
				}
			}

			if dividers != 1 {
				t.Errorf("width %d rendered %d iteration rows, want 1: %q", width, dividers, m.committedWrapped)
			}
		})
	}
}

func TestRewrapOnResize(t *testing.T) {
	m := sized(t, 120, 30)

	m.appendEntry(strings.Repeat("word ", 60))

	m.render()

	wide := m.committedWrapped

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 40, Height: 30})

	narrow := updated.(model).committedWrapped

	if wide == narrow {
		t.Error("resizing should re-wrap the committed log")
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

// The badge is how an operator tells at a glance whether the run is still going
// or has failed, so the three statuses have to be distinguishable - a
// non-empty badge that says the same thing for all three tells them nothing.
func TestBadgeReflectsStatus(t *testing.T) {
	badges := map[status]string{}

	for _, st := range []status{statusRunning, statusDone, statusFailed} {
		m := sized(t, 80, 24)

		m.status = st

		badge := m.badge()

		if badge == "" {
			t.Errorf("status %v produced no badge", st)
		}

		for other, seen := range badges {
			if seen == badge {
				t.Errorf("statuses %v and %v render the same badge %q", other, st, badge)
			}
		}

		badges[st] = badge
	}

	// and each says which state it is, in words rather than colour alone
	for st, want := range map[status]string{
		statusRunning: "working",
		statusDone:    "done",
		statusFailed:  "failed",
	} {
		if !strings.Contains(badges[st], want) {
			t.Errorf("the %v badge %q does not say %q", st, badges[st], want)
		}
	}
}

func TestFooterShowsTheKeyHints(t *testing.T) {
	m := sized(t, 100, 30)

	m.iteration = 3

	footer := m.footer()

	if footer == "" {
		t.Fatal("the footer must render while running")
	}

	// the log is read-only, so the only affordances are scrolling and quitting
	for _, want := range []string{"scroll", "quit"} {
		if !strings.Contains(footer, want) {
			t.Errorf("the footer should mention %q: %q", want, footer)
		}
	}
}

// The outcome leaves the UI as an error the caller can act on, which is how a
// non-zero exit code reaches the shell.
func TestExitBecomesAnError(t *testing.T) {
	m := sized(t, 100, 30)

	m.finish(loop.Result{Reason: loop.StopCycle, Message: "kept repeating"})

	err := m.runError()

	if err == nil {
		t.Fatal("a failed run must surface as an error")
	}

	if !strings.Contains(err.Error(), "kept repeating") {
		t.Errorf("the error must carry the outcome: %v", err)
	}

	clean := sized(t, 100, 30)

	clean.finish(loop.Result{Reason: loop.StopSettled, Message: "done"})

	if err := clean.runError(); err != nil {
		t.Errorf("a settled run must not error: %v", err)
	}
}

// A spinner tick is ignored once the run has ended, or the finished screen keeps
// animating.
func TestSpinnerStopsWhenTheRunEnds(t *testing.T) {
	m := sized(t, 80, 24)

	m.status = statusDone

	_, cmd := m.Update(tickMsg{})

	if cmd != nil {
		t.Error("the clock must stop once the run has ended")
	}
}

// The renderers have to know the real tool names. A mismatch is not a compile
// error - it just quietly renders the agent's most-used tool as an anonymous
// key/value dump, which is how `shell` went unstyled.
func TestRenderToolStartCoversTheBuiltInTools(t *testing.T) {
	tests := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"shell", map[string]any{"command": "go test ./..."}, "go test"},

		// a caller's own tool still renders, just generically
		{"custom", map[string]any{"thing": "value"}, "thing=value"},
	}

	for _, test := range tests {
		got := stripANSI(renderToolStart(test.tool, test.args))

		if !strings.Contains(got, test.want) {
			t.Errorf("renderToolStart(%q) = %q, want it to contain %q", test.tool, got, test.want)
		}
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
		{"shell echoes its output", "shell", "hello\nworld", true, "hello"},
		{"a silent command still confirms", "shell", "", true, "done"},
		{"an unknown tool echoes", "custom", "some output", true, "some output"},
		{"an unknown tool with nothing to say", "custom", "", false, ""},
		{"a non-string, non-map result", "shell", 42, false, ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := stripANSI(renderToolEnd(test.tool, test.result))

			if !test.wantAny {
				if got != "" {
					t.Errorf("expected nothing, got %q", got)
				}

				return
			}

			if !strings.Contains(got, test.want) {
				t.Errorf("renderToolEnd = %q, want it to contain %q", got, test.want)
			}
		})
	}
}

// One record must not scroll the rest of the run off the screen: it is cut at a
// third of the terminal's height, the last row an ellipsis.
func TestARecordIsClippedToAThirdOfTheTerminalHeight(t *testing.T) {
	var lines []string

	for i := 0; i < 50; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}

	m := sized(t, 100, 30)

	m.handleEvent(loop.Event{Kind: loop.EventToolCallEnd, Tool: "shell", Result: strings.Join(lines, "\n")})

	rows := strings.Split(stripANSI(m.committedWrapped), "\n")

	if len(rows) != 10 {
		t.Fatalf("the record took %d rows on a 30-row terminal, want 10:\n%s", len(rows), strings.Join(rows, "\n"))
	}

	if !strings.Contains(rows[len(rows)-1], "…") {
		t.Errorf("the cut must end on an ellipsis, got %q", rows[len(rows)-1])
	}

	if !strings.Contains(rows[0], "done") || !strings.Contains(rows[1], "line 0") {
		t.Errorf("the head of the record must survive:\n%s", strings.Join(rows, "\n"))
	}
}

// Rows are what count, not source lines: a few long lines wrap into many rows.
func TestAWrappedRecordIsClippedByRows(t *testing.T) {
	m := sized(t, 40, 30)

	long := strings.Repeat("word ", 40)

	m.handleEvent(loop.Event{Kind: loop.EventToolCallEnd, Tool: "shell", Result: long + "\n" + long + "\n" + long})

	if got := len(strings.Split(m.committedWrapped, "\n")); got != 10 {
		t.Errorf("a wrapped record took %d rows, want the 10 a third of 30 allows", got)
	}
}

// A record that fits is left exactly as it is, with no ellipsis.
func TestARecordThatFitsIsNotClipped(t *testing.T) {
	m := sized(t, 100, 30)

	m.handleEvent(loop.Event{Kind: loop.EventToolCallEnd, Tool: "shell", Result: "one\ntwo\nthree"})

	got := stripANSI(m.committedWrapped)

	if strings.Contains(got, "…") || !strings.Contains(got, "three") {
		t.Errorf("a short record must be shown whole:\n%s", got)
	}
}

// The limit follows the terminal: growing the window shows more of a record that
// was cut, because the log is re-wrapped from the full record.
func TestResizingChangesHowMuchOfARecordShows(t *testing.T) {
	var lines []string

	for i := 0; i < 50; i++ {
		lines = append(lines, "line")
	}

	m := sized(t, 100, 30)

	m.handleEvent(loop.Event{Kind: loop.EventToolCallEnd, Tool: "shell", Result: strings.Join(lines, "\n")})

	before := len(strings.Split(m.committedWrapped, "\n"))

	resized, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 60})
	m = resized.(model)

	if after := len(strings.Split(m.committedWrapped, "\n")); after != 20 || after <= before {
		t.Errorf("rows after growing the window = %d (was %d), want 20", after, before)
	}
}

func TestRenderToolEndHandlesStructuredResults(t *testing.T) {
	failure := stripANSI(renderToolEnd("shell", map[string]any{
		"success": false,
		"error":   "exit status 1",
		"stderr":  "compile failed",
	}))

	if !strings.Contains(failure, "exit status 1") {
		t.Errorf("a structured failure must surface: %q", failure)
	}

	success := stripANSI(renderToolEnd("shell", map[string]any{"stdout": "all good"}))

	if !strings.Contains(success, "all good") {
		t.Errorf("structured output must surface: %q", success)
	}
}

// A key nobody bound must reach the viewport rather than being swallowed, and
// must not quit: an unattended run ended by a stray keystroke is a lost run.
func TestUnboundKeysAreNotQuitKeys(t *testing.T) {
	for _, key := range []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune{'x'}},
		{Type: tea.KeyEsc},
		{Type: tea.KeyEnter},
		{Type: tea.KeyRunes, Runes: []rune{' '}},
	} {
		m := sized(t, 80, 24)

		updated, _ := m.Update(key)

		// a quit would leave the model unchanged and end the program; what we
		// can check without running the program is that the viewer is still
		// there and still running
		if updated.(model).status != statusRunning {
			t.Errorf("key %v ended the run", key)
		}
	}
}

// The spinner and the clock stop once the run ends, so a finished screen does
// not look like it is still working.
func TestTheClockStopsWhenTheRunEnds(t *testing.T) {
	m := sized(t, 80, 24)

	m.status = statusDone

	before := m.elapsed

	next, cmd := m.Update(tickMsg{})

	if cmd != nil {
		t.Error("a finished run must not schedule another tick")
	}

	if next.(model).elapsed != before {
		t.Error("the clock kept running after the run ended")
	}
}

// The footer tells the operator how to leave. While a run is going it shows the
// keys; once it is over it says so explicitly, because the run no longer ends
// on its own.
func TestFooterAddsAnExitHintWhenTheRunIsOver(t *testing.T) {
	m := sized(t, 80, 24)

	m.status = statusRunning

	running := m.footer()

	m.status = statusDone

	finished := m.footer()

	if len(finished) <= len(running) {
		t.Errorf("a finished footer must say more than a running one:\n%q\n%q", finished, running)
	}

	if !strings.Contains(finished, "exit") {
		t.Errorf("the finished footer must say how to leave: %q", finished)
	}
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
		if got := fmtDuration(test.duration); got != test.want {
			t.Errorf("fmtDuration(%s) = %q, want %q", test.duration, got, test.want)
		}
	}
}

// The header has to survive a terminal too narrow for it. A run watched over a
// phone-sized ssh session is still a run.
func TestTitleBarSurvivesANarrowTerminal(t *testing.T) {
	for _, width := range []int{1, 8, 12, 20} {
		m := sized(t, width, 24)

		title := m.titleBar()

		if title == "" {
			t.Errorf("width %d produced no title bar", width)
		}

		if strings.Contains(title, "\n") {
			t.Errorf("width %d wrapped the title bar: %q", width, title)
		}
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

		// a task or tool argument in CJK or emoji was cut mid-rune, so the meta
		// bar and the tool lines rendered a replacement character - and the cap
		// counted bytes, so the line was cut far short of the width it was given
		{in: "日本語のタスク説明文です", max: 6, want: "日本語のタ…"},
		{in: "🚀🚀🚀🚀🚀", max: 3, want: "🚀🚀…"},
		{in: "日本語", max: 10, want: "日本語"},
	}

	for _, test := range tests {
		if got := truncate(test.in, test.max); got != test.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", test.in, test.max, got, test.want)
		}
	}
}

// A shell tool reports failure on stderr, and that is exactly the output an
// operator reading a failed run needs to see.
func TestCommandOutputPrefersStdoutButFallsBackToStderr(t *testing.T) {
	if got := commandOutput(map[string]any{"stdout": "all good\n"}); !strings.Contains(got, "all good") {
		t.Errorf("stdout was not rendered: %q", got)
	}

	got := commandOutput(map[string]any{"stdout": "", "stderr": "permission denied\n"})

	if !strings.Contains(got, "permission denied") {
		t.Errorf("stderr was not rendered when stdout was empty: %q", got)
	}

	if got := commandOutput(map[string]any{}); got != "" {
		t.Errorf("a silent command rendered %q, want nothing", got)
	}
}

func TestActivityLogIsBoundedForLongRuns(t *testing.T) {
	// a not-yet-sized model: render() no-ops, so this exercises the scrollback cap
	// without the per-append viewport cost (which is what a real, model-paced run
	// pays anyway, now bounded to the cap)
	m := newModel("do the thing", "m", "b", "d")

	limit := m.maxEntries          // DefaultMaxScrollback
	total := limit + limit/4 + 200 // enough to force a trim past the cap + slack
	for i := 0; i < total; i++ {
		m.appendEntry(fmt.Sprintf("line %d", i))
	}

	// bounded: never more than the cap plus the trim slack, whatever the run length
	if len(m.entries) > limit+limit/4 {
		t.Fatalf("scrollback must stay bounded, got %d entries", len(m.entries))
	}

	if !m.truncated {
		t.Error("truncation must be flagged once the cap is exceeded")
	}

	// the newest line always survives
	if got := m.entries[len(m.entries)-1]; got != fmt.Sprintf("line %d", total-1) {
		t.Errorf("the most recent line must be kept, got %q", got)
	}

	// the oldest kept line is exactly total - len(entries), and older ones are gone
	oldest := total - len(m.entries)
	if got := m.entries[0]; got != fmt.Sprintf("line %d", oldest) {
		t.Errorf("the oldest kept line should be line %d, got %q", oldest, got)
	}
}

// When the log has been trimmed, the viewer must say so and point at the session
// log, so a watcher knows the on-screen history is not the whole run.
func TestTrimmedLogShowsAMarker(t *testing.T) {
	m := sized(t, 100, 30)
	m.truncated = true
	m.appendEntry("a recent line") // triggers a render

	if !strings.Contains(m.vp.View(), "trimmed") {
		t.Errorf("a trimmed log must show a marker, got:\n%s", m.vp.View())
	}
}

// The scrollback cap is configurable (Meta.MaxScrollback / ui.scrollback): a
// caller can keep fewer or more lines than the default.
func TestScrollbackCapIsConfigurable(t *testing.T) {
	m := newModel("t", "m", "b", "d")
	m.maxEntries = 50 // what Run sets from Meta.MaxScrollback

	for i := 0; i < 300; i++ {
		m.appendEntry(fmt.Sprintf("line %d", i))
	}

	if len(m.entries) > m.maxEntries+m.maxEntries/4 {
		t.Errorf("a custom cap of %d must be honoured, kept %d", m.maxEntries, len(m.entries))
	}

	if len(m.entries) < m.maxEntries {
		t.Errorf("should keep about the cap %d, kept only %d", m.maxEntries, len(m.entries))
	}
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

	for _, label := range []string{"provider", "model", "iter", "elapsed", "tokens", "dir"} {
		at := strings.Index(bar, label)
		if at < 0 || at < last {
			t.Fatalf("%q is missing or out of order in %q", label, bar)
		}

		last = at
	}
}

// metaSegments splits a rendered bar into its visible segments, so a test can
// ask what is actually on screen without knowing how a segment is built.
func metaSegments(bar string) []string {
	var out []string

	for _, part := range strings.Split(stripANSI(bar), "·") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}

	return out
}

// A stat segment is shown whole or not at all. Clipping the bar to the terminal
// width left whichever segment straddled the edge half-rendered - a truncated
// count can be misread, which is worse than an absent one - so a segment that
// does not fit is dropped entirely.
//
// The narrow renders are checked against a wide one rather than against
// hardcoded text: what a segment says is the bar's business, and a test that
// restated it would fail on every wording change while still not proving
// anything about fitting.
func TestMetaBarDropsSegmentsThatDoNotFitWhole(t *testing.T) {
	reference := metaSegments(sized(t, 400, 30).metaBar())

	if len(reference) != headerSegments {
		t.Fatalf("a wide terminal must show every header segment: %q", reference)
	}

	for _, width := range []int{12, 20, 33, 47, 68, 95, 140} {
		m := sized(t, width, 30)

		bar := m.metaBar()

		// nothing may spill past the terminal edge
		if got := lipgloss.Width(bar); got > width {
			t.Errorf("at width %d the bar is %d columns wide: %q", width, got, bar)
		}

		shown := metaSegments(bar)

		// every segment on screen is one of the whole segments, not a prefix of
		// one - this is the half-visible bug, stated directly
		for i, segment := range shown {
			if i >= len(reference) {
				t.Errorf("at width %d the bar grew segments it should not have: %q", width, shown)

				break
			}

			if segment != reference[i] {
				t.Errorf("at width %d segment %d is %q, want the whole %q",
					width, i, segment, reference[i])
			}
		}

		// and what is shown is a prefix of the configured order, so segments
		// appear and disappear predictably as the terminal is resized
		if len(shown) > len(reference) {
			t.Errorf("at width %d the bar shows %d segments, more than exist", width, len(shown))
		}
	}
}

// Narrowing the terminal only ever removes segments, and widening only ever
// adds them back - the bar is a prefix that grows monotonically with the space
// it has, which is what makes a resize readable rather than a reshuffle.
func TestMetaBarGrowsMonotonicallyWithWidth(t *testing.T) {
	previous := -1

	for width := 8; width <= 400; width += 4 {
		shown := len(metaSegments(sized(t, width, 30).metaBar()))

		if shown < previous {
			t.Fatalf("widening to %d columns dropped a segment (%d, was %d)", width, shown, previous)
		}

		previous = shown
	}

	if previous != headerSegments {
		t.Errorf("the widest terminal shows %d segments, want all %d", previous, headerSegments)
	}
}

// A terminal too narrow for even the first segment shows an empty bar rather
// than a fragment of one.
func TestMetaBarIsEmptyWhenNothingFits(t *testing.T) {
	m := sized(t, 3, 30)

	if bar := m.metaBar(); strings.TrimSpace(stripANSI(bar)) != "" {
		t.Errorf("nothing fits at 3 columns, so nothing should be drawn: %q", bar)
	}
}

// The header shows the provider-reported token usage, and progress against any
// configured limits (5/1000); a limit that is unset shows no denominator.
func TestMetaBarShowsTokensAndLimits(t *testing.T) {
	m := sized(t, 400, 30)
	m.iteration = 5
	m.maxIterations = 1000
	m.maxDuration = 30 * time.Minute
	m.inputTokens = 32000
	m.outputTokens = 13000

	bar := m.metaBar()

	if !strings.Contains(bar, "5/1000") {
		t.Errorf("iter must show progress against its limit: %q", bar)
	}

	if !strings.Contains(bar, "/30:00") {
		t.Errorf("elapsed must show the time limit: %q", bar)
	}

	if !strings.Contains(bar, "32.0k") || !strings.Contains(bar, "13.0k") {
		t.Errorf("tokens must show provider usage compactly: %q", bar)
	}
}

// A usage update from the run sets the counts the meta bar reads.
func TestHandleEventRecordsUsage(t *testing.T) {
	m := sized(t, 100, 30)

	m.handleEvent(loop.Event{Kind: loop.EventUsage, InputTokens: 1234, OutputTokens: 567})

	if m.inputTokens != 1234 || m.outputTokens != 567 {
		t.Errorf("usage not recorded: in=%d out=%d", m.inputTokens, m.outputTokens)
	}
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
		if got := fmtTokens(tc.n); got != tc.want {
			t.Errorf("fmtTokens(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// The error behind a failed run is kept and shown - it is usually the run's only
// diagnostic (the provider's 404, not the loop's "the provider failed").
func TestTheErrorBehindAFailedRunIsKeptAndShown(t *testing.T) {
	m := sized(t, 100, 30)

	next, _ := m.Update(doneMsg{result: loop.Result{
		Reason:  loop.StopError,
		Message: "the provider failed",
		Err:     fmt.Errorf("provider: Model 'x' not found (404)"),
	}})
	m = next.(model)

	if err := m.runError(); err == nil || !strings.Contains(err.Error(), "not found (404)") {
		t.Errorf("runError() = %v, want the provider's own words", err)
	}

	joined := strings.Join(m.entries, "\n")
	if !strings.Contains(joined, "not found (404)") {
		t.Errorf("the log should show the underlying error:\n%s", joined)
	}
}

// A retry spends a continuation and then waits out a backoff; without a
// rendered line the wait shows as empty iteration dividers stacking up - a run
// that is surviving looks like one that is hanging.
func TestRetryEventIsRendered(t *testing.T) {
	m := sized(t, 100, 30)

	m.handleEvent(loop.Event{Kind: loop.EventRetry, Text: "provider: Provider returned error: ERROR (upstream: Stealth) (400)"})

	joined := strings.Join(m.entries, "\n")
	if !strings.Contains(joined, "retrying") || !strings.Contains(joined, "Stealth") {
		t.Errorf("the retry should be visible with its cause:\n%s", joined)
	}
}

// The header shows the order's title when it has one. The task is the whole
// order rendered for the model - objective, criteria and constraints - so a
// one-line header of it is a paragraph cut mid-word, which is exactly what
// titles exist to replace.
func TestTitleBarPrefersTheTitleOverTheTask(t *testing.T) {
	task := "add rate limiting to the api\n\nAcceptance criteria - the objective is not met until every one of these holds:\n1. the suite passes"

	withTitle := sized(t, 120, 30)
	withTitle.task = task
	withTitle.title = "Rate limiting"

	bar := stripANSI(withTitle.titleBar())

	if !strings.Contains(bar, "Rate limiting") {
		t.Errorf("the title bar should show the title: %q", bar)
	}

	if strings.Contains(bar, "add rate limiting to the api") {
		t.Errorf("the task text should give way to the title: %q", bar)
	}

	// without a title there is still something to show
	untitled := sized(t, 120, 30)
	untitled.task = task

	if bar := stripANSI(untitled.titleBar()); !strings.Contains(bar, "add rate limiting") {
		t.Errorf("an untitled run must fall back to the task: %q", bar)
	}
}

// A live value growing a digit - nine iterations becoming ten, 999 tokens
// becoming 1.0k - must not shove the segments after it sideways: a header that
// jitters on every tick is unreadable at a glance, which is the only way a
// header is read. Each volatile field is followed by a fixed one, and the test
// asks whether that fixed one moved.
func TestMetaBarDoesNotShiftAsValuesChange(t *testing.T) {
	tests := []struct {
		name   string
		next   string // the label of the segment after the one that changes
		before func(*model)
		after  func(*model)
	}{
		{
			name:   "iterations gaining a digit",
			next:   "elapsed",
			before: func(m *model) { m.iteration = 9 },
			after:  func(m *model) { m.iteration = 10 },
		},
		{
			name:   "iterations against a limit",
			next:   "elapsed",
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

			test.before(&m)

			was := strings.Index(stripANSI(m.metaBar()), test.next)

			test.after(&m)

			if now := strings.Index(stripANSI(m.metaBar()), test.next); now != was {
				t.Errorf("the change moved %q from column %d to %d:\n%q", test.next, was, now, stripANSI(m.metaBar()))
			}
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

	return map[string]any{"tasks": list}
}

// The task list is the one piece of the run worth reading in full, so it renders
// as a checklist: what is done, what is under way, what is left, what is stuck.
func TestRenderTasksShowsTheChecklist(t *testing.T) {
	out := stripANSI(renderToolStart("tasks", tasksArgs(
		[3]string{"read the handler", "done", ""},
		[3]string{"add validation", "in_progress", "the error path is missing"},
		[3]string{"write a test", "pending", ""},
		[3]string{"deploy", "blocked", "needs credentials"},
	)))

	for _, want := range []string{
		"tasks", "1/4 done",
		"✓ read the handler", "▶ add validation", "· write a test", "✗ deploy",
		"the error path is missing", "needs credentials",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered tasks missing %q:\n%s", want, out)
		}
	}
}

// A call the tool refuses - no tasks, an unknown status - still shows its header
// rather than crashing the render, and draws nothing it cannot vouch for.
func TestRenderTasksIsRobust(t *testing.T) {
	for name, args := range map[string]map[string]interface{}{
		"no arguments":  {},
		"an empty list": {"tasks": []interface{}{}},
		"a bad status":  tasksArgs([3]string{"a", "started", ""}),
		"not a list":    {"tasks": "do it"},
		"a non-object":  {"tasks": []interface{}{"do it"}},
	} {
		out := stripANSI(renderToolStart("tasks", args))

		if !strings.Contains(out, "tasks") {
			t.Errorf("%s: should still render a header: %q", name, out)
		}

		if strings.Contains(out, "done") {
			t.Errorf("%s: a refused list must not report progress: %q", name, out)
		}
	}
}
