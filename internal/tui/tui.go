// Package tui renders the read-only terminal view of an autonomous agent run. It has no text input, since the user watches
// the agent and does not drive it. Everything on screen derives from the engine's event stream and the run's ending.
package tui

import (
	"context"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"

	"github.com/s3rj1k/agent/internal/loop"
)

// Meta is the header display information shown above the activity log.
type Meta struct {
	// Task is the one-line instruction the agent is working on.
	Task string

	// An optional short label for the work (an order's title or its file name), shown instead of the task text,
	// which is the whole order rendered to prose. Empty falls back to Task.
	Title string
	// Model is the model name driving the agent.
	Model string
	// Provider is the name of the model provider the run targets.
	Provider string
	// Workdir is the directory the agent's tools operate in.
	Workdir string

	// The configured run limits, shown as "5/1000" progress in the meta bar. Zero means unbounded or not worth
	// showing (such as the default iteration fallback), so no denominator appears. The iteration limit also sizes
	// the scrollback.
	MaxIterations int
	MaxDuration   time.Duration
}

// IsInteractive reports whether stdout is a terminal capable of the full-screen
// viewer. The viewer is the only way agent shows a run, so a caller checks this
// before starting anything.
func IsInteractive() bool {
	fd := os.Stdout.Fd()

	return isatty.IsTerminal(fd)
}

// RunViewer owns the viewer's lifetime. It starts the run, hands the program to start, and shuts the run down once start
// returns. Start and programOptions are seams for tests, which cannot open a terminal and need a headless program that
// still consumes messages.
func RunViewer(
	ctx context.Context,
	m *Model,
	engine *loop.Engine,
	start func(*tea.Program) (tea.Model, error),
	programOptions ...tea.ProgramOption,
) (loop.Result, error) {
	// Quitting the viewer stops the agent, not only the watching. The agent has shell and file-write access, so
	// a run left going with nothing on screen would keep editing the working tree unseen.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	p := tea.NewProgram(m, append([]tea.ProgramOption{tea.WithAltScreen()}, programOptions...)...)

	done := make(chan struct{})
	results := make(chan loop.Result, 1)

	go RunAgent(ctx, p, engine, results, done)

	final, err := start(p)

	// The run must conclude before this returns. Quitting cancels it and the engine ends with its aborted outcome,
	// which the caller records right after, so returning early would race it out of the log. The timeout only bounds a hung engine.
	cancel()

	var result loop.Result

	select {
	case <-done:
		result = <-results
	case <-time.After(3 * time.Second):
	}

	if err != nil {
		return result, err
	}

	switch m := final.(type) {
	case *Model:
		return result, m.RunError()
	default:
		return result, nil
	}
}

// Run renders the read-only TUI while the agent executes. It owns the Bubble Tea program lifecycle and blocks until the user
// quits or the program errors, with the agent talking to the UI through tea messages. It returns the run's Result along with
// any error, empty when the run never began or was abandoned still going.
func Run(ctx context.Context, meta Meta, opts *loop.Options) (loop.Result, error) {
	engine, err := loop.New(opts)
	if err != nil {
		return loop.Result{}, err
	}

	m := NewModel(meta.Task, meta.Model, meta.Provider, meta.Workdir)

	m.Title = meta.Title
	m.MaxEntries = Scrollback(meta.MaxIterations)
	m.MaxIterations = meta.MaxIterations
	m.MaxDuration = meta.MaxDuration

	return RunViewer(ctx, m, engine, func(p *tea.Program) (tea.Model, error) { return p.Run() })
}
