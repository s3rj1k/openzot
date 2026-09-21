package main

import (
	"fmt"

	"github.com/s3rj1k/agent/internal/conversation"
	"github.com/s3rj1k/agent/internal/failure"
	"github.com/s3rj1k/agent/internal/loop"
	"github.com/s3rj1k/agent/internal/session"
)

// A recorder writes a run into a session log as the engine hands it over. The engine knows nothing of files. A line that
// cannot be written is not ignored, since the log is the agent's long-term memory and the operator's only record. The first
// failure is kept and reported through the newRecorder callback, so the caller can end the run.
type recorder struct {
	writer    *session.Writer
	onFailure func(error)

	// recorded is how many messages of the conversation are in the log already.
	recorded int

	// failed is the first write that did not go through.
	failed error
}

// The newRecorder function wraps a writer. The callback, when set, is called once, with the first
// write that fails.
func newRecorder(writer *session.Writer, onFailure func(error)) *recorder {
	return &recorder{writer: writer, onFailure: onFailure}
}

// Err is the first write that failed, or nil if every one went through.
func (r *recorder) Err() error { return r.failed }

// wrote keeps the first failure and reports it.
func (r *recorder) wrote(err error) {
	if err == nil || r.failed != nil {
		return
	}

	r.failed = err

	if r.onFailure != nil {
		r.onFailure(err)
	}
}

// Conversation records what the conversation has gained since the last call. It only ever grows, so this takes the whole
// of it and writes the unseen tail. Called with the seed messages, it records them ahead of the run, so a session that dies
// in its first turn still says what it was asked to do.
func (r *recorder) Conversation(messages []conversation.Message) {
	for r.recorded < len(messages) {
		r.wrote(r.writer.Message(messages[r.recorded]))

		r.recorded++
	}
}

// Event records something that happened. Token-by-token narration is dropped, since the finished message carries the
// same content and keeping it would make the log ten times larger.
func (r *recorder) Event(event loop.Event) { //nolint:gocritic // hugeParam: it is loop.Options.OnEvent, which takes the event by value
	if event.Kind == loop.EventToken || event.Kind == loop.EventReasoningToken {
		return
	}

	text := event.Text

	// A usage event carries its numbers in fields the log has no column for. Render them into the text, or
	// the log records a usage event that says nothing about why a run cost 567k tokens.
	if event.Kind == loop.EventUsage && text == "" {
		text = fmt.Sprintf("input %d output %d", event.InputTokens, event.OutputTokens)
	}

	r.wrote(r.writer.Event(session.Event{Kind: string(event.Kind), Tool: event.Tool, Text: text, Iteration: event.Iteration}))
}

// Result records the ending. The last of the conversation, then the outcome, which
// closes the log.
func (r *recorder) Result(result *loop.Result) {
	// the run's last turn happened after the final hand-over, so the ending is
	// written down here
	r.Conversation(result.Messages)

	var cause string

	if result.Err != nil {
		cause = result.Err.Error()
	}

	r.wrote(r.writer.Result(session.Result{
		Reason:        string(result.Reason),
		Message:       result.Message,
		Error:         cause,
		Failure:       failure.EvidenceOf(result.Err),
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
