package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/s3rj1k/agent/internal/loop"
)

// These messages are how the background run talks to the Bubble Tea program. The
// UI never calls the engine directly. It only reacts to these.
type (
	// One event of the run, carried as it happens.
	EventMsg struct{ Event loop.Event }

	// The run's ending. It is always the last message, and it carries
	// everything the run has to say about how it ended. The reason, what the
	// terminal tool wrote, and - on a failure - the error behind it.
	DoneMsg struct{ Result loop.Result }
)

// RunAgent runs the autonomous agent to completion in its own goroutine, relaying every event into the program and then its
// ending, which is also handed to results for the caller to read once done is closed. All the autonomy lives in the engine,
// so this is a pure pump, event in and tea.Msg out.
func RunAgent(ctx context.Context, p *tea.Program, engine *loop.Engine, results chan<- loop.Result, done chan<- struct{}) {
	defer close(done)

	result := engine.Run(ctx, func(event loop.Event) {
		p.Send(EventMsg{Event: event})
	})

	results <- result

	p.Send(DoneMsg{Result: result})
}
