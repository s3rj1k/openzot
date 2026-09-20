package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/openzot/openzot/internal/conversation"
)

// readLog reads a log the way anyone does: one JSON value per line. A line that
// is not JSON fails the test, which is the point - the format is the contract.
func readLog(t *testing.T, path string) []Record {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}

	if len(data) > 0 && data[len(data)-1] != '\n' {
		t.Fatalf("log does not end on a line: %q", data[max(0, len(data)-40):])
	}

	var records []Record

	for i, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}

		var record Record

		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("line %d is not a JSON record: %v\n%s", i+1, err, line)
		}

		records = append(records, record)
	}

	return records
}

func kinds(records []Record) []Kind {
	out := make([]Kind, 0, len(records))

	for _, record := range records {
		out = append(out, record.Kind)
	}

	return out
}

func TestOpenCreatesTheLogAndItsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", ".zot", "orders", "1758300000.jsonl")

	writer, err := Open(path, Meta{Task: "add a health endpoint", Model: "m", Provider: "p", Workdir: "/w"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	defer writer.Close()

	if writer.Path() != path {
		t.Errorf("Path = %q, want %q", writer.Path(), path)
	}

	records := readLog(t, path)

	if len(records) != 1 || records[0].Kind != KindMeta || records[0].Meta == nil {
		t.Fatalf("a new log should open with exactly its meta record: %+v", records)
	}

	meta := records[0].Meta

	if meta.Task != "add a health endpoint" || meta.Model != "m" || meta.Provider != "p" || meta.Workdir != "/w" {
		t.Errorf("meta = %+v", meta)
	}

	if records[0].At.IsZero() {
		t.Error("every record is stamped with when it was written")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// transcripts of a run can hold anything the agent read, so the file is the
	// operator's alone
	if info.Mode().Perm() != 0o600 {
		t.Errorf("log mode = %v, want 0600", info.Mode().Perm())
	}
}

// Every step of a run is one line. A log a person reads with cat and jq has to
// be complete and well-formed at every point, not only when the run is over.
func TestEveryKindOfStepIsOneJSONLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	writer, err := Open(path, Meta{Task: "t"})
	if err != nil {
		t.Fatal(err)
	}

	steps := []func() error{
		func() error { return writer.Message(conversation.Message{Type: "user", Text: "go"}) },
		func() error { return writer.Message(conversation.Message{Type: "reasoning", Text: "think\nabout\nit"}) },
		func() error {
			return writer.Message(conversation.Message{Type: "activity", Activity: &conversation.Activity{
				Kind: "request", ID: "c1", Name: "shell", Arguments: `{"command":"ls"}`,
			}})
		},
		func() error { return writer.Event(Event{Kind: "toolCallStart", Tool: "shell", Iteration: 1}) },
		func() error { return writer.Result(Result{Reason: "settled", Iterations: 1}) },
	}

	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("step %d: %v", i+1, err)
		}
	}

	got := kinds(readLog(t, path))

	want := []Kind{KindMeta, KindMessage, KindMessage, KindMessage, KindEvent, KindResult}

	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("records = %v, want %v", got, want)
	}

	// a message with newlines in it is still one line: the newline is escaped
	data, _ := os.ReadFile(path)

	if lines := bytes.Count(data, []byte("\n")); lines != len(want) {
		t.Errorf("the log has %d lines, want one per record (%d)", lines, len(want))
	}
}

// The log is append-only. Each record lands after everything already there, and
// nothing before it is ever rewritten - so a reader tailing the file, or a copy
// taken mid-run, is always a prefix of what comes next.
func TestNothingAlreadyWrittenIsEverChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	writer, err := Open(path, Meta{Task: "t"})
	if err != nil {
		t.Fatal(err)
	}

	previous, _ := os.ReadFile(path)

	for i := 0; i < 20; i++ {
		var err error

		if i%2 == 0 {
			err = writer.Message(conversation.Message{Type: "bot", Text: fmt.Sprintf("message %d", i)})
		} else {
			err = writer.Event(Event{Kind: "iteration", Iteration: i})
		}

		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}

		current, _ := os.ReadFile(path)

		if !bytes.HasPrefix(current, previous) || len(current) <= len(previous) {
			t.Fatalf("after record %d the log is not the previous log plus one more line", i)
		}

		previous = current
	}
}

// A record is on disk when the call that wrote it returns, not when the run
// ends: what a killed run leaves behind is everything it had recorded.
func TestARecordIsOnDiskAsSoonAsItIsWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	writer, err := Open(path, Meta{Task: "t"})
	if err != nil {
		t.Fatal(err)
	}

	defer writer.Close()

	if err := writer.Message(conversation.Message{Type: "reasoning", Text: "the model's own words"}); err != nil {
		t.Fatal(err)
	}

	// read while the writer is still open, as a tail or a crash would
	records := readLog(t, path)

	last := records[len(records)-1]

	if last.Kind != KindMessage || last.Message.Type != "reasoning" || last.Message.Text != "the model's own words" {
		t.Errorf("the last record on disk = %+v", last)
	}
}

// Running the task again appends to the same file. The earlier run is kept
// byte for byte, and the new one opens with a meta record of its own, so the
// file is the whole history of the task, one run after another.
func TestARunAppendsToTheLogInsteadOfReplacingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	first, err := Open(path, Meta{Task: "the brief"})
	if err != nil {
		t.Fatal(err)
	}

	_ = first.Message(conversation.Message{Type: "user", Text: "first run"})
	_ = first.Result(Result{Reason: "settled"})

	before, _ := os.ReadFile(path)

	second, err := Open(path, Meta{Task: "the brief"})
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}

	_ = second.Message(conversation.Message{Type: "user", Text: "second run"})
	_ = second.Result(Result{Reason: "failed"})

	after, _ := os.ReadFile(path)

	if !bytes.HasPrefix(after, before) {
		t.Fatal("the second run changed what the first run wrote")
	}

	got := kinds(readLog(t, path))
	want := []Kind{KindMeta, KindMessage, KindResult, KindMeta, KindMessage, KindResult}

	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("records = %v, want two runs one after the other: %v", got, want)
	}
}

// A run killed mid-write leaves a torn last line. The next run must not glue its
// meta record onto it: that would lose the new run's opening and leave one line
// that is neither.
func TestATornFinalLineIsEndedBeforeTheNextRunStarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	first, err := Open(path, Meta{Task: "t"})
	if err != nil {
		t.Fatal(err)
	}

	_ = first.Message(conversation.Message{Type: "user", Text: "before the kill"})
	_ = first.Close()

	// what a kill mid-write leaves: half a record, no newline
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := file.WriteString(`{"kind":"message","at":"2026-09-19T10:00:00Z","message":{"type":"bo`); err != nil {
		t.Fatal(err)
	}

	_ = file.Close()

	second, err := Open(path, Meta{Task: "t"})
	if err != nil {
		t.Fatalf("Open over a torn line: %v", err)
	}

	_ = second.Result(Result{Reason: "settled"})

	data, _ := os.ReadFile(path)

	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")

	// the torn line is still there, on its own, and every line after it parses
	if !strings.Contains(lines[2], `"type":"bo`) || strings.Contains(lines[2], `"kind":"meta"`) {
		t.Fatalf("the torn line should stand alone: %q", lines[2])
	}

	for i, line := range lines[3:] {
		var record Record

		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Errorf("line %d after the torn one is not a record: %v: %s", i+4, err, line)
		}
	}

	if !strings.Contains(lines[3], `"kind":"meta"`) {
		t.Errorf("the new run should open on its own line with its meta: %q", lines[3])
	}
}

// A log that already ends on a line gets no extra blank line.
func TestACleanLogIsNotPaddedBeforeTheNextRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	first, _ := Open(path, Meta{Task: "t"})
	_ = first.Result(Result{Reason: "settled"})

	second, err := Open(path, Meta{Task: "t"})
	if err != nil {
		t.Fatal(err)
	}

	_ = second.Close()

	data, _ := os.ReadFile(path)

	if strings.Contains(string(data), "\n\n") {
		t.Errorf("a blank line crept in between runs:\n%s", data)
	}
}

// The engine emits events from its own goroutine while the caller records
// messages. Two records must never share a line.
func TestConcurrentWritesNeverInterleave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	writer, err := Open(path, Meta{Task: "t"})
	if err != nil {
		t.Fatal(err)
	}

	const writers, each = 20, 25

	var wg sync.WaitGroup

	for w := 0; w < writers; w++ {
		wg.Add(1)

		go func(w int) {
			defer wg.Done()

			for i := 0; i < each; i++ {
				text := strings.Repeat(fmt.Sprintf("w%d-%d ", w, i), 40)

				if err := writer.Message(conversation.Message{Type: "bot", Text: text}); err != nil {
					t.Errorf("write: %v", err)
				}
			}
		}(w)
	}

	wg.Wait()

	_ = writer.Close()

	if got := len(readLog(t, path)); got != 1+writers*each {
		t.Errorf("the log has %d records, want the meta plus %d messages", got, writers*each)
	}
}

func TestAResultClosesTheLogAndLaterWritesAreRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	writer, _ := Open(path, Meta{Task: "t"})

	if err := writer.Result(Result{Reason: "settled"}); err != nil {
		t.Fatalf("Result: %v", err)
	}

	if err := writer.Message(conversation.Message{Type: "user", Text: "too late"}); err == nil {
		t.Error("writing after the result must be an error, not a silent drop")
	}

	if got := len(readLog(t, path)); got != 2 {
		t.Errorf("the log has %d records, want just the meta and the result", got)
	}

	if err := writer.Close(); err != nil {
		t.Errorf("Close after Result: %v", err)
	}
}

func TestOpenReportsAnUnusableLocation(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "a-file")

	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	// a directory that cannot be made, and a path that is itself a directory
	if _, err := Open(filepath.Join(blocker, "sub", "task.jsonl"), Meta{}); err == nil {
		t.Error("a log under a file must not open")
	}

	if _, err := Open(t.TempDir(), Meta{}); err == nil {
		t.Error("a directory is not a log")
	}
}

// An activity round-trips through the log with its whole payload: the arguments
// verbatim, the result as the tool returned it.
func TestAToolCallIsRecordedInFull(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	writer, _ := Open(path, Meta{Task: "t"})

	_ = writer.Message(conversation.Message{Type: "activity", Activity: &conversation.Activity{
		Kind: "response", ID: "call_1", Name: "shell", Arguments: `{"command":"go test ./..."}`, Result: "ok",
	}})

	_ = writer.Close()

	records := readLog(t, path)

	activity := records[1].Message.Activity

	if activity == nil || activity.ID != "call_1" || activity.Name != "shell" ||
		activity.Arguments != `{"command":"go test ./..."}` || activity.Result != "ok" {
		t.Errorf("activity = %+v", activity)
	}
}
