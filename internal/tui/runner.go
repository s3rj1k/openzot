package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/openzot/openzot/internal/loop"
)

// These messages are how the background run talks to the Bubble Tea program. The
// UI never calls the engine directly. It only reacts to these.
type (
	// One event of the run, carried as it happens.
	eventMsg struct{ ev loop.Event }

	// The run's ending. It is always the last message, and it carries
	// everything the run has to say about how it ended. The reason, what the
	// terminal tool wrote, and - on a failure - the error behind it.
	doneMsg struct{ result loop.Result }
)

// runAgent runs the autonomous agent to completion, relaying every event into
// the program and then its ending. It is meant to be launched in its own
// goroutine. It blocks until the run is over. The ending is also handed to results,
// which the caller reads once done is closed.
//
// All the autonomy lives in the engine - it loops the model through
// plan/act/observe/exit on its own. RunAgent is a pure pump. Event in, tea.Msg out.
func runAgent(ctx context.Context, p *tea.Program, engine *loop.Engine, results chan<- loop.Result, done chan<- struct{}) {
	defer close(done)

	result := engine.Run(ctx, func(event loop.Event) {
		p.Send(eventMsg{ev: event})
	})

	results <- result

	p.Send(doneMsg{result: result})
}
