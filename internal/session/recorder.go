package session

import (
	"fmt"

	"github.com/openzot/openzot/internal/conversation"
	"github.com/openzot/openzot/internal/loop"
	"github.com/openzot/openzot/internal/provider"
)

// Recorder writes a run into a session log as the engine hands it over.
//
// The engine knows nothing about files; it gives away the conversation, its events
// and its result, and this decides where they land. That is what keeps a session
// log an operational concern rather than something the loop has to carry.
//
// A line that cannot be written is not ignored: the log is the agent's long-term
// memory and the operator's only record, so a run that has stopped being recorded
// has stopped being what it was asked to be. The first failure is kept, and the
// callback given to NewRecorder is told of it, so the caller can end the run.
type Recorder struct {
	writer    *Writer
	onFailure func(error)

	// recorded is how many messages of the conversation are in the log already.
	recorded int

	// failed is the first write that did not go through.
	failed error
}

// NewRecorder wraps a writer. OnFailure, when set, is called once, with the first
// write that fails.
func NewRecorder(writer *Writer, onFailure func(error)) *Recorder {
	return &Recorder{writer: writer, onFailure: onFailure}
}

// Err is the first write that failed, or nil if every one went through.
func (r *Recorder) Err() error { return r.failed }

// wrote keeps the first failure and reports it.
func (r *Recorder) wrote(err error) {
	if err == nil || r.failed != nil {
		return
	}

	r.failed = err

	if r.onFailure != nil {
		r.onFailure(err)
	}
}

// Conversation records what the conversation has gained since the last call.
//
// The conversation only ever grows - what is sent to the model is trimmed to the
// window, but the history itself is never rewritten - so this is called with the
// whole of it and writes the tail it has not seen. Called once with the messages
// a run is seeded with, it records them ahead of the run, so a session that dies
// in its first turn still says what it was asked to do.
func (r *Recorder) Conversation(messages []conversation.Message) {
	for r.recorded < len(messages) {
		r.wrote(r.writer.Message(messages[r.recorded]))

		r.recorded++
	}
}

// Event records something that happened.
//
// Token-by-token narration is deliberately dropped: it is the same content the
// finished message already carries, and keeping it would make the log an order
// of magnitude larger for nothing.
func (r *Recorder) Event(event loop.Event) {
	if event.Kind == loop.EventToken || event.Kind == loop.EventReasoningToken {
		return
	}

	text := event.Text

	// a usage event carries its numbers in dedicated fields the log has no column
	// for; render them into the text, or the log records a usage event that says
	// nothing - and "why did this run cost 567k tokens" is exactly the question a
	// session has to answer after the fact
	if event.Kind == loop.EventUsage && text == "" {
		text = fmt.Sprintf("input %d output %d", event.InputTokens, event.OutputTokens)
	}

	r.wrote(r.writer.Event(Event{Kind: string(event.Kind), Tool: event.Tool, Text: text, Iteration: event.Iteration}))
}

// Result records the ending: the last of the conversation, then the outcome, which
// closes the log.
func (r *Recorder) Result(result loop.Result) {
	// the run's last turn happened after the final hand-over, so the ending is
	// written down here
	r.Conversation(result.Messages)

	var cause string

	if result.Err != nil {
		cause = result.Err.Error()
	}

	r.wrote(r.writer.Result(Result{
		Reason:        string(result.Reason),
		Message:       result.Message,
		Error:         cause,
		Failure:       provider.FailureOf(result.Err),
		Code:          result.ExitCode(),
		Iterations:    result.Budget.Iterations,
		Calls:         result.Budget.Calls,
		Continuations: result.Budget.Recoveries,
		Cycles:        result.Budget.Cycles,
		Settles:       result.Budget.Settles,
		InputTokens:   result.Budget.InputTokens,
		OutputTokens:  result.Budget.OutputTokens,
	}))
}
