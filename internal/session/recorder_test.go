package session

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openzot/openzot/internal/agent"
	"github.com/openzot/openzot/internal/loop"
)

// The model's reasoning is part of the record: the scratchpad is often the only
// place that says why it did what it did. It arrives as its own message, kept in
// order between what the user said and what the model answered.
func TestTheModelsReasoningIsRecordedInOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	writer, err := Open(path, Meta{Task: "add a health endpoint"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	recorder := NewRecorder(writer)

	for _, message := range []agent.Message{
		{Type: agent.TypeUser, Text: "add a health endpoint"},
		{Type: agent.TypeReasoning, Text: "I should look at the router first,\nthen add the handler."},
		{Type: agent.TypeBot, Text: "on it"},
	} {
		if err := recorder.RecordMessage(message); err != nil {
			t.Fatalf("RecordMessage: %v", err)
		}
	}

	records := readLog(t, path)

	var got []string

	for _, record := range records[1:] {
		got = append(got, record.Message.Type+": "+record.Message.Text)
	}

	want := []string{
		"user: add a health endpoint",
		"reasoning: I should look at the router first,\nthen add the handler.",
		"bot: on it",
	}

	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("messages = %q, want %q", got, want)
	}
}

func TestARunIsRecordedFromItsFirstMessageToItsOutcome(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	writer, err := Open(path, Meta{Task: "add a health endpoint"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	recorder := NewRecorder(writer)

	if err := recorder.RecordMessage(agent.Message{Type: agent.TypeUser, Text: "add a health endpoint"}); err != nil {
		t.Fatalf("RecordMessage: %v", err)
	}

	if err := recorder.RecordEvent("toolCallStart", "shell", "go test ./...", 1); err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}

	if err := recorder.RecordMessage(agent.Message{
		Type: agent.TypeActivity,
		Text: "ok",
		Activity: &agent.Activity{
			Kind:      agent.ActivityResponse,
			ID:        "call_1",
			Name:      "shell",
			Arguments: `{"command":"go test ./..."}`,
			Result:    "ok",
		},
	}); err != nil {
		t.Fatalf("RecordMessage: %v", err)
	}

	if err := recorder.RecordResult(agent.Summary{
		Reason:       "stop",
		Message:      "finished",
		Iterations:   3,
		Calls:        2,
		Cycles:       1,
		Settles:      1,
		InputTokens:  1200,
		OutputTokens: 340,
	}); err != nil {
		t.Fatalf("RecordResult: %v", err)
	}

	records := readLog(t, path)

	got := kinds(records)
	want := []Kind{KindMeta, KindMessage, KindEvent, KindMessage, KindResult}

	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("records = %v, want %v", got, want)
	}

	// the type has to survive as the string the agent's own type names
	if records[1].Message.Type != string(agent.TypeUser) || records[3].Message.Type != string(agent.TypeActivity) {
		t.Errorf("message types = %q, %q", records[1].Message.Type, records[3].Message.Type)
	}

	activity := records[3].Message.Activity

	if activity == nil || activity.Kind != string(agent.ActivityResponse) || activity.ID != "call_1" ||
		activity.Name != "shell" || activity.Arguments != `{"command":"go test ./..."}` || activity.Result != "ok" {
		t.Errorf("the call was not recorded whole: %+v", activity)
	}

	if event := records[2].Event; event.Kind != "toolCallStart" || event.Tool != "shell" || event.Iteration != 1 {
		t.Errorf("event = %+v", event)
	}

	result := records[4].Result

	if result.Reason != "stop" || result.Iterations != 3 || result.Settles != 1 || result.InputTokens != 1200 || result.OutputTokens != 340 {
		t.Errorf("result = %+v", result)
	}
}

// Token events are the same text the finished message already carries. Keeping
// them would multiply the size of every log for nothing.
func TestTokenNarrationIsNotRecorded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	writer, _ := Open(path, Meta{Task: "t"})

	recorder := NewRecorder(writer)

	for _, kind := range []string{"token", "reasoningToken"} {
		if err := recorder.RecordEvent(kind, "", "hello", 1); err != nil {
			t.Fatalf("RecordEvent(%q): %v", kind, err)
		}
	}

	_ = recorder.RecordEvent("iteration", "", "", 1)

	_ = writer.Close()

	records := readLog(t, path)

	if got := kinds(records); fmt.Sprint(got) != fmt.Sprint([]Kind{KindMeta, KindEvent}) {
		t.Fatalf("records = %v, want the meta and the one real event", got)
	}

	if records[1].Event.Kind != "iteration" {
		t.Errorf("event = %+v", records[1].Event)
	}
}

// A recorder with nowhere to write is a run that is not recorded, and it has to
// be silent rather than an error the run has to handle.
func TestANilRecorderIsHarmless(t *testing.T) {
	var recorder *Recorder

	if err := recorder.RecordMessage(agent.Message{Type: agent.TypeUser}); err != nil {
		t.Errorf("RecordMessage: %v", err)
	}

	if err := recorder.RecordEvent("iteration", "", "", 1); err != nil {
		t.Errorf("RecordEvent: %v", err)
	}

	if err := recorder.RecordResult(agent.Summary{}); err != nil {
		t.Errorf("RecordResult: %v", err)
	}

	empty := NewRecorder(nil)

	if err := empty.RecordMessage(agent.Message{Type: agent.TypeUser}); err != nil {
		t.Errorf("RecordMessage on an empty recorder: %v", err)
	}

	if err := empty.RecordResult(agent.Summary{}); err != nil {
		t.Errorf("RecordResult on an empty recorder: %v", err)
	}
}

func TestRecordResultKeepsTheUnderlyingError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	writer, err := Open(path, Meta{Task: "x"})
	if err != nil {
		t.Fatal(err)
	}

	recorder := NewRecorder(writer)

	if err := recorder.RecordResult(agent.Summary{
		Reason:  "error",
		Message: "the provider failed",
		Error:   "provider: Model 'stealth/ox-alpha' not found (404)",
		Failure: &loop.Failure{Status: 404, ResponseBody: `{"error":{"message":"not found"}}`, RequestBytes: 118234},
		Code:    1,
	}); err != nil {
		t.Fatalf("RecordResult: %v", err)
	}

	records := readLog(t, path)

	result := records[len(records)-1].Result

	if result.Error != "provider: Model 'stealth/ox-alpha' not found (404)" {
		t.Errorf("Error = %q, want the provider's own words", result.Error)
	}

	failure := result.Failure
	if failure == nil || failure.Status != 404 || failure.RequestBytes != 118234 || !strings.Contains(failure.ResponseBody, "not found") {
		t.Errorf("Failure = %+v, want the wire evidence kept verbatim", failure)
	}
}
