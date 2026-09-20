// Package tui renders the read-only terminal view of an autonomous agent run.
//
// The UI deliberately has no text input: the user watches the agent work, they
// do not drive it. Everything on screen is derived from the event stream the
// engine emits - tool calls, iterations, token narration - and the run's ending.
package tui

import (
	"context"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"

	"github.com/openzot/openzot/internal/loop"
)

// Meta is the header display information shown above the activity log.
type Meta struct {
	// Task is the one-line instruction the agent is working on.
	Task string

	// Title is an optional short label for the work - a work order's title, or
	// one derived from its file name. When set it is shown instead of the task
	// text, which is the whole order rendered to prose and reads as a truncated
	// paragraph in a one-line header. Empty falls back to Task.
	Title string
	// Model is the model name driving the agent.
	Model string
	// Provider is the name of the model provider the run targets.
	Provider string
	// Workdir is the directory the agent's tools operate in.
	Workdir string

	// MaxScrollback caps how many log lines the viewer keeps on screen. Zero uses
	// DefaultMaxScrollback; a larger value keeps more history (at more memory).
	// The full run is always in the session log regardless.
	MaxScrollback int

	// MaxIterations and MaxDuration are the configured run limits, shown
	// as "5/1000" progress in the meta bar. Zero means unbounded (or not worth
	// showing, e.g. the default iteration backstop), so no denominator appears.
	MaxIterations int
	MaxDuration   time.Duration
}

// IsInteractive reports whether stdout is a terminal capable of the full-screen
// viewer. The viewer is the only way zot shows a run, so a caller checks this
// before starting anything.
func IsInteractive() bool {
	fd := os.Stdout.Fd()

	return isatty.IsTerminal(fd)
}

// runViewer owns the viewer's lifetime: it starts the run, hands the program to
// start, and shuts the run down once start returns. Start is a seam for tests,
// which cannot open a terminal - Run passes (*tea.Program).Run. ProgramOptions is
// a seam for tests, which cannot open a terminal and need a headless program that
// still consumes messages.
func runViewer(
	ctx context.Context,
	m *model,
	engine *loop.Engine,
	start func(*tea.Program) (tea.Model, error),
	programOptions ...tea.ProgramOption,
) (loop.Result, error) {
	// Quitting the viewer stops the agent rather than merely stopping watching
	// it. The agent has shell and file-write access, so a process
	// that returned from here with the run still going would leave something
	// editing the working tree with nothing on screen reporting what it does.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	p := tea.NewProgram(m, append([]tea.ProgramOption{tea.WithAltScreen()}, programOptions...)...)

	done := make(chan struct{})
	results := make(chan loop.Result, 1)

	go runAgent(ctx, p, engine, results, done)

	final, err := start(p)

	// The run must conclude before this returns: quitting cancels it, and the
	// engine then ends with its aborted outcome - which the caller records into the
	// session as soon as this function returns, so returning immediately would race
	// that ending out of the log. Cancel explicitly, then give the engine a bounded
	// moment to finish; the timeout only exists so a pathologically hung engine
	// cannot hold the terminal hostage.
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
	case *model:
		return result, m.runError()
	default:
		return result, nil
	}
}

// Run renders the read-only TUI while the autonomous agent executes. It owns the
// Bubble Tea program lifecycle and blocks until the user quits or the program
// errors. The agent runs in the background and communicates with the UI solely
// through tea messages.
//
// Along with any error it returns the run's Result, so a caller can report how it
// ended - and what it spent - without scraping the screen. The result is empty
// when the run never began, or was still going when it was abandoned.
func Run(ctx context.Context, meta Meta, opts *loop.Options) (loop.Result, error) {
	engine, err := loop.New(opts)
	if err != nil {
		return loop.Result{}, err
	}

	m := newModel(meta.Task, meta.Model, meta.Provider, meta.Workdir)

	m.title = meta.Title
	if meta.MaxScrollback > 0 {
		m.maxEntries = meta.MaxScrollback
	}

	m.maxIterations = meta.MaxIterations
	m.maxDuration = meta.MaxDuration

	return runViewer(ctx, m, engine, func(p *tea.Program) (tea.Model, error) { return p.Run() })
}
