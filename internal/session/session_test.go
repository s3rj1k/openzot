package session_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/conversation"
	"github.com/openzot/openzot/internal/session"
)

// readLog reads a log the way anyone does. One JSON value per line. A line that
// is not JSON fails the test, which is the point - the format is the contract.
func readLog(t *testing.T, path string) []session.Record {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	if len(data) > 0 {
		require.Equal(t, byte('\n'), data[len(data)-1], "log does not end on a line")
	}

	var records []session.Record

	for i, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}

		var record session.Record

		err := json.Unmarshal([]byte(line), &record)
		require.NoError(t, err, "line %d is not a JSON record: %v\n%s", i+1, err, line)

		records = append(records, record)
	}

	return records
}

func kinds(records []session.Record) []session.Kind {
	out := make([]session.Kind, 0, len(records))

	for _, record := range records {
		out = append(out, record.Kind)
	}

	return out
}

func TestOpenCreatesTheLogAndItsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", ".agent", "orders", "1758300000.jsonl")

	writer, err := session.Open(path, session.Meta{Task: litAddAHealthEndpoint, Model: "m", Provider: "p", Workdir: "/w"})
	require.NoError(t, err)

	defer writer.Close()

	assert.Equal(t, path, writer.Path())

	records := readLog(t, path)

	require.Len(t, records, 1, "a new log should open with exactly its meta record")
	require.Equal(t, session.KindMeta, records[0].Kind, "a new log should open with exactly its meta record")
	require.NotNil(t, records[0].Meta, "a new log should open with exactly its meta record")

	meta := records[0].Meta

	assert.Equal(t, litAddAHealthEndpoint, meta.Task)
	assert.Equal(t, "m", meta.Model)
	assert.Equal(t, "p", meta.Provider)
	assert.Equal(t, "/w", meta.Workdir)

	assert.False(t, records[0].At.IsZero(), "every record is stamped with when it was written")

	info, err := os.Stat(path)
	require.NoError(t, err)

	// transcripts of a run can hold anything the agent read, so the file is the
	// operator's alone
	assert.EqualValues(t, 0o600, info.Mode().Perm())
}

// Every step of a run is one line. A log a person reads with cat and jq has to
// be complete and well-formed at every point, not only when the run is over.
func TestEveryKindOfStepIsOneJSONLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	writer, err := session.Open(path, session.Meta{Task: "t"})
	require.NoError(t, err)

	steps := []func() error{
		func() error { return writer.Message(conversation.Message{Type: litUser, Text: "go"}) },
		func() error {
			return writer.Message(conversation.Message{Type: litReasoning, Text: "think\nabout\nit"})
		},
		func() error {
			return writer.Message(conversation.Message{Type: litActivity, Activity: &conversation.Activity{
				Kind: "request", ID: "c1", Name: litShell, Arguments: litCommandLs,
			}})
		},
		func() error { return writer.Event(session.Event{Kind: "toolCallStart", Tool: litShell, Iteration: 1}) },
		func() error { return writer.Result(session.Result{Reason: litSettled, Iterations: 1}) },
	}

	for i, step := range steps {
		require.NoError(t, step(), "step %d", i+1)
	}

	got := kinds(readLog(t, path))

	want := []session.Kind{session.KindMeta, session.KindMessage, session.KindMessage, session.KindMessage, session.KindEvent, session.KindResult}

	assert.Equal(t, fmt.Sprint(want), fmt.Sprint(got))

	// a message with newlines in it is still one line. The newline is escaped
	data, _ := os.ReadFile(path)

	lines := bytes.Count(data, []byte("\n"))
	assert.Equal(t, len(want), lines, "the log has %d lines, want one per record (%d)", lines, len(want))
}

// The log is append-only. Each record lands after everything already there, and
// nothing before it is ever rewritten - so a reader tailing the file, or a copy
// taken mid-run, is always a prefix of what comes next.
func TestNothingAlreadyWrittenIsEverChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	writer, err := session.Open(path, session.Meta{Task: "t"})
	require.NoError(t, err)

	previous, _ := os.ReadFile(path)

	for i := range 20 {
		var err error

		if i%2 == 0 {
			err = writer.Message(conversation.Message{Type: "bot", Text: fmt.Sprintf("message %d", i)})
		} else {
			err = writer.Event(session.Event{Kind: "iteration", Iteration: i})
		}

		require.NoError(t, err)

		current, _ := os.ReadFile(path)

		require.True(t, bytes.HasPrefix(current, previous), "after record %d the log is not the previous log plus one more line", i)
		require.Greater(t, len(current), len(previous), "after record %d the log is not the previous log plus one more line", i)

		previous = current
	}
}

// A record is on disk when the call that wrote it returns, not when the run
// ends. What a killed run leaves behind is everything it had recorded.
func TestARecordIsOnDiskAsSoonAsItIsWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	writer, err := session.Open(path, session.Meta{Task: "t"})
	require.NoError(t, err)

	defer writer.Close()

	require.NoError(t, writer.Message(conversation.Message{Type: litReasoning, Text: "the model's own words"}))

	// read while the writer is still open, as a tail or a crash would
	records := readLog(t, path)

	last := records[len(records)-1]

	assert.Equal(t, session.KindMessage, last.Kind)
	assert.EqualValues(t, litReasoning, last.Message.Type)
	assert.Equal(t, "the model's own words", last.Message.Text)
}

// Running the task again appends to the same file. The earlier run is kept
// byte for byte, and the new one opens with a meta record of its own, so the
// file is the whole history of the task, one run after another.
func TestARunAppendsToTheLogInsteadOfReplacingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	first, err := session.Open(path, session.Meta{Task: "the brief"})
	require.NoError(t, err)

	_ = first.Message(conversation.Message{Type: litUser, Text: "first run"})
	_ = first.Result(session.Result{Reason: litSettled})

	before, _ := os.ReadFile(path)

	second, err := session.Open(path, session.Meta{Task: "the brief"})
	require.NoError(t, err)

	_ = second.Message(conversation.Message{Type: litUser, Text: "second run"})
	_ = second.Result(session.Result{Reason: "failed"})

	after, _ := os.ReadFile(path)

	require.True(t, bytes.HasPrefix(after, before), "the second run changed what the first run wrote")

	got := kinds(readLog(t, path))
	want := []session.Kind{session.KindMeta, session.KindMessage, session.KindResult, session.KindMeta, session.KindMessage, session.KindResult}

	assert.Equal(t, fmt.Sprint(want), fmt.Sprint(got), "records = %v, want two runs one after the other", got)
}

// A run killed mid-write leaves a torn last line. The next run must not glue its
// meta record onto it. That would lose the new run's opening and leave one line
// that is neither.
func TestATornFinalLineIsEndedBeforeTheNextRunStarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	first, err := session.Open(path, session.Meta{Task: "t"})
	require.NoError(t, err)

	_ = first.Message(conversation.Message{Type: litUser, Text: "before the kill"})
	_ = first.Close()

	// what a kill mid-write leaves. Half a record, no newline
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	require.NoError(t, err)

	_, err = file.WriteString(`{"kind":"message","at":"2026-09-19T10:00:00Z","message":{"type":"bo`)
	require.NoError(t, err)

	_ = file.Close()

	second, err := session.Open(path, session.Meta{Task: "t"})
	require.NoError(t, err, "Open over a torn line")

	_ = second.Result(session.Result{Reason: litSettled})

	data, _ := os.ReadFile(path)

	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")

	// the torn line is still there, on its own, and every line after it parses
	require.Contains(t, lines[2], `"type":"bo`, "the torn line should stand alone")
	require.NotContains(t, lines[2], `"kind":"meta"`, "the torn line should stand alone")

	for i, line := range lines[3:] {
		var record session.Record

		err := json.Unmarshal([]byte(line), &record)
		require.NoError(t, err, "line %d after the torn one is not a record: %v: %s", i+4, err, line)
	}

	assert.Contains(t, lines[3], `"kind":"meta"`, "the new run should open on its own line with its meta")
}

// A log that already ends on a line gets no extra blank line.
func TestACleanLogIsNotPaddedBeforeTheNextRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	first, _ := session.Open(path, session.Meta{Task: "t"})
	_ = first.Result(session.Result{Reason: litSettled})

	second, err := session.Open(path, session.Meta{Task: "t"})
	require.NoError(t, err)

	_ = second.Close()

	data, _ := os.ReadFile(path)

	assert.NotContains(t, string(data), "\n\n", "a blank line crept in between runs")
}

// The engine emits events from its own goroutine while the caller records
// messages. Two records must never share a line.
func TestConcurrentWritesNeverInterleave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	writer, err := session.Open(path, session.Meta{Task: "t"})
	require.NoError(t, err)

	const writers, each = 20, 25

	var wg sync.WaitGroup

	for w := range writers {
		wg.Go(func() {
			for i := range each {
				text := strings.Repeat(fmt.Sprintf("w%d-%d ", w, i), 40)

				require.NoError(t, writer.Message(conversation.Message{Type: "bot", Text: text}))
			}
		})
	}

	wg.Wait()

	_ = writer.Close()

	got := len(readLog(t, path))
	assert.Equal(t, 1+writers*each, got, "the log has %d records, want the meta plus %d messages", got, writers*each)
}

func TestAResultClosesTheLogAndLaterWritesAreRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	writer, _ := session.Open(path, session.Meta{Task: "t"})

	require.NoError(t, writer.Result(session.Result{Reason: litSettled}))

	require.Error(t, writer.Message(conversation.Message{Type: litUser, Text: "too late"}), "writing after the result must be an error, not a silent drop")

	assert.Len(t, readLog(t, path), 2, "want just the meta and the result")

	require.NoError(t, writer.Close(), "Close after Result")
}

func TestOpenReportsAnUnusableLocation(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "a-file")

	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))

	// a directory that cannot be made, and a path that is itself a directory
	_, err := session.Open(filepath.Join(blocker, "sub", "task.jsonl"), session.Meta{})
	require.Error(t, err, "a log under a file must not open")

	_, err = session.Open(t.TempDir(), session.Meta{})
	require.Error(t, err)
}

// An activity round-trips through the log with its whole payload. The arguments
// verbatim, the result as the tool returned it.
func TestAToolCallIsRecordedInFull(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	writer, _ := session.Open(path, session.Meta{Task: "t"})

	_ = writer.Message(conversation.Message{Type: litActivity, Activity: &conversation.Activity{
		Kind: "response", ID: litCall1, Name: litShell, Arguments: litCommandGoTest, Result: "ok",
	}})

	_ = writer.Close()

	records := readLog(t, path)

	activity := records[1].Message.Activity

	assert.NotNil(t, activity)
	assert.Equal(t, litCall1, activity.ID)
	assert.Equal(t, litShell, activity.Name)
	assert.JSONEq(t, litCommandGoTest, activity.Arguments)
	assert.Equal(t, "ok", activity.Result)
}
