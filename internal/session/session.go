// Package session records a run to disk, one JSON record per line, append-only, one log per task. Each record is written
// whole and synced before the next, so a crashed run leaves everything up to the crash and the log is readable mid-run.
// Zot never reads a log back. It is a record for a person, and the agent's long-term memory when its window forgets.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/openzot/openzot/internal/conversation"
	"github.com/openzot/openzot/internal/provider"
)

// Kind identifies a record.
type Kind string

const (
	// KindMeta opens a run in the log. The task, model and settings it started
	// with. One per run, always the first record of it.
	KindMeta Kind = "meta"

	// KindMessage is one conversation message, in the order it was said.
	KindMessage Kind = "message"

	// KindEvent is something that happened - a tool call, a retry, a nudge.
	// Kept because it is what explains a run afterwards.
	KindEvent Kind = "event"

	// KindResult closes a run with the outcome. A run with none - the next
	// KindMeta, or the end of the file, comes first - did not finish, which is
	// itself worth knowing.
	KindResult Kind = "result"
)

// Record is one line of a session log.
type Record struct {
	Kind Kind      `json:"kind"`
	At   time.Time `json:"at"`

	// Meta is set on a KindMeta record.
	Meta *Meta `json:"meta,omitempty"`

	// Message is set on a KindMessage record.
	Message *conversation.Message `json:"message,omitempty"`

	// Event is set on a KindEvent record.
	Event *Event `json:"event,omitempty"`

	// Result is set on a KindResult record.
	Result *Result `json:"result,omitempty"`
}

// Meta describes how a run started.
type Meta struct {
	// Task is the brief the run was given.
	Task string `json:"task"`

	Model string `json:"model"`
	// Provider is the selected named connection.
	Provider string `json:"provider"`

	// Workdir is where the agent's tools operated.
	Workdir string `json:"workdir"`
}

// Event is something that happened during the run.
type Event struct {
	Kind      string `json:"kind"`
	Tool      string `json:"tool,omitempty"`
	Text      string `json:"text,omitempty"`
	Iteration int    `json:"iteration,omitzero"`
}

// Result is how a run ended.
type Result struct {
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`

	// The underlying failure on an error ending, in the provider's own words. "the provider failed" answers
	// nothing at three in the morning, but the 404 naming the wrong model does.
	Error string `json:"error,omitempty"`

	// The wire evidence behind Error when the failure was a provider response. An opaque upstream "ERROR"
	// and a context-length message read the same in Error, but the rejected request's size tells them apart.
	Failure *provider.Failure `json:"failure,omitempty"`

	Code int `json:"code"`

	Iterations    int `json:"iterations"`
	Calls         int `json:"calls"`
	Continuations int `json:"continuations"`
	Cycles        int `json:"cycles"`
	Settles       int `json:"settles"`

	// The provider-billed totals for the run, so an audit can see cost and not only the terminal that
	// produced it.
	InputTokens  int `json:"inputTokens,omitzero"`
	OutputTokens int `json:"outputTokens,omitzero"`
}

// Writer appends records to a session log. It is safe for concurrent use, since the engine emits events from its own
// goroutine while the caller may be recording messages.
type Writer struct {
	mu   sync.Mutex
	file *os.File
	path string
}

// endTornLine starts a fresh line when the file does not end on one.
func (w *Writer) endTornLine() error {
	info, err := w.file.Stat()
	if err != nil {
		return fmt.Errorf("open session log: %w", err)
	}

	if info.Size() == 0 {
		return nil
	}

	last := make([]byte, 1)

	if _, err := w.file.ReadAt(last, info.Size()-1); err != nil {
		return fmt.Errorf("open session log: %w", err)
	}

	if last[0] == '\n' {
		return nil
	}

	if _, err := w.file.Write([]byte{'\n'}); err != nil {
		return fmt.Errorf("open session log: %w", err)
	}

	return nil
}

// write appends one record as a single line and syncs it. One write on an append-only file keeps records from
// interleaving, and syncing per record is on purpose, since buffering would lose exactly the tail that explains a failure.
func (w *Writer) write(record Record) error {
	line, err := json.Marshal(record)
	if err != nil {
		return err
	}

	line = append(line, '\n')

	w.mu.Lock()

	defer w.mu.Unlock()

	if w.file == nil {
		return errors.New("session: log is closed")
	}

	if _, err := w.file.Write(line); err != nil {
		return err
	}

	return w.file.Sync()
}

// Open starts a run in the log at path, appending if it exists and creating it and its directory if not. Whatever an
// earlier run left is never touched, and a line torn by a killed run is ended first so the new meta record starts
// on a line of its own.
func Open(path string, meta Meta) (*Writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create session directory: %w", err)
	}

	// read access is only for looking at the last byte. Every write appends
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600) //nolint:gosec // G304: the session log path is the one the run was given
	if err != nil {
		return nil, fmt.Errorf("open session log: %w", err)
	}

	writer := &Writer{file: file, path: path}

	if err := writer.endTornLine(); err != nil {
		file.Close()

		return nil, err
	}

	if err := writer.write(Record{Kind: KindMeta, At: time.Now().UTC(), Meta: &meta}); err != nil {
		file.Close()

		return nil, err
	}

	return writer, nil
}

// Path returns the log file.
func (w *Writer) Path() string { return w.path }

// Message records a conversation entry.
func (w *Writer) Message(message conversation.Message) error {
	return w.write(Record{Kind: KindMessage, At: time.Now().UTC(), Message: &message})
}

// Event records something that happened.
func (w *Writer) Event(event Event) error {
	return w.write(Record{Kind: KindEvent, At: time.Now().UTC(), Event: &event})
}

// Close releases the file. Safe to call twice.
func (w *Writer) Close() error {
	w.mu.Lock()

	defer w.mu.Unlock()

	if w.file == nil {
		return nil
	}

	err := w.file.Close()

	w.file = nil

	return err
}

// Result records the outcome and closes the log.
func (w *Writer) Result(result Result) error {
	if err := w.write(Record{Kind: KindResult, At: time.Now().UTC(), Result: &result}); err != nil {
		return err
	}

	return w.Close()
}
