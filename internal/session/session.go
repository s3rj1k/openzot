// Package session records a run to disk, one JSON record per line.
//
// An autonomous run is unattended by definition: nobody watched it, and by the
// time anyone looks the terminal is gone. A session log is what turns "it
// failed overnight" into something answerable - what it tried, what the tools
// returned, and where it stopped.
//
// There is one log per task, and it is append-only. A run opens with a meta
// record and adds a record for every message, event and the outcome, each as one
// line written in a single call and synced to disk before the next; nothing is
// ever rewritten, truncated or reordered. Running the task again appends a new
// run to the same file rather than replacing the last, so the file is the whole
// history of the task, one run after another. Because a line is the unit, a log
// is readable while the run is still going, and a crashed run leaves everything
// up to the crash: at worst the final line is torn, and the next run starts on a
// fresh one.
//
// zot itself never reads a log back, and a run is not resumed from one. It is a
// record for a person, with `cat` and `jq` - and the agent is told where its own
// is, so what its context window has forgotten it can look up: the log is its
// long-term memory.
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
	// KindMeta opens a run in the log: the task, model and settings it started
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

	// Error is the underlying failure on an error ending - the provider's own
	// words, not the loop's summary of them. "the provider failed" answers
	// nothing at three in the morning; the 404 naming the wrong model does.
	Error string `json:"error,omitempty"`

	// Failure is the wire evidence behind Error, when the failure was a
	// provider response. The raw exchange is what troubleshooting needs: an
	// opaque upstream "ERROR" and a proper context-length message read the
	// same in Error, but the refused request's size tells them apart.
	Failure *provider.Failure `json:"failure,omitempty"`

	Code int `json:"code"`

	Iterations    int `json:"iterations"`
	Calls         int `json:"calls"`
	Continuations int `json:"continuations"`
	Cycles        int `json:"cycles"`
	Settles       int `json:"settles"`

	// InputTokens and OutputTokens are the provider-billed totals for the run,
	// persisted so an audit can see cost rather than only the terminal that
	// produced it.
	InputTokens  int `json:"inputTokens,omitzero"`
	OutputTokens int `json:"outputTokens,omitzero"`
}

// Writer appends records to a session log.
//
// Safe for concurrent use: the engine emits events from its own goroutine while
// the caller may be recording messages.
type Writer struct {
	mu   sync.Mutex
	file *os.File
	path string
}

// Open starts a run in the log at path, appending to the file if it exists and
// creating it, and its directory, if not.
//
// The file is opened append-only: whatever an earlier run left is never touched.
// If that run was killed mid-line, the torn line is ended first, so the new
// run's meta record starts on a line of its own instead of being glued to the
// wreckage of the last.
func Open(path string, meta Meta) (*Writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create session directory: %w", err)
	}

	// read access is only for looking at the last byte; every write appends
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
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

// Path returns the log file.
func (w *Writer) Path() string { return w.path }

// write appends one record as a single line and syncs it.
//
// The line is built whole and handed to the kernel in one write on an
// append-only file, so a record is never interleaved with another and never
// lands anywhere but the end. Synced per record on purpose: the log has to be
// readable while the run is in flight, and a crashed run has to leave
// everything up to the crash. Buffering would lose exactly the tail that
// explains a failure.
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

// Message records a conversation entry.
func (w *Writer) Message(message conversation.Message) error {
	return w.write(Record{Kind: KindMessage, At: time.Now().UTC(), Message: &message})
}

// Event records something that happened.
func (w *Writer) Event(event Event) error {
	return w.write(Record{Kind: KindEvent, At: time.Now().UTC(), Event: &event})
}

// Result records the outcome and closes the log.
func (w *Writer) Result(result Result) error {
	if err := w.write(Record{Kind: KindResult, At: time.Now().UTC(), Result: &result}); err != nil {
		return err
	}

	return w.Close()
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
