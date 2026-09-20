package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"

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

	recorder := NewRecorder(writer, nil)

	recorder.Conversation([]loop.Message{
		{Type: loop.TypeUser, Text: "add a health endpoint"},
		{Type: loop.TypeReasoning, Text: "I should look at the router first,\nthen add the handler."},
		{Type: loop.TypeBot, Text: "on it"},
	})

	records := readLog(t, path)

	var got []string

	for _, record := range records[1:] {
		got = append(got, string(record.Message.Type)+": "+record.Message.Text)
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

	recorder := NewRecorder(writer, nil)

	conversation := []loop.Message{{Type: loop.TypeUser, Text: "add a health endpoint"}}

	recorder.Conversation(conversation)

	recorder.Event(loop.Event{Kind: loop.EventToolCallStart, Tool: "shell", Text: "go test ./...", Iteration: 1})

	conversation = append(conversation, loop.Message{
		Type: loop.TypeActivity,
		Text: "ok",
		Activity: &loop.Activity{
			Kind:      loop.ActivityResponse,
			ID:        "call_1",
			Name:      "shell",
			Arguments: `{"command":"go test ./..."}`,
			Result:    "ok",
		},
	})

	recorder.Result(loop.Result{
		Reason:   loop.StopSettled,
		Message:  "finished",
		Messages: conversation,
		Budget:   loop.Budget{Iterations: 3, Calls: 2, Cycles: 1, Settles: 1, InputTokens: 1200, OutputTokens: 340},
	})

	records := readLog(t, path)

	got := kinds(records)
	want := []Kind{KindMeta, KindMessage, KindEvent, KindMessage, KindResult}

	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("records = %v, want %v", got, want)
	}

	// the type has to survive as the string the engine's own type names
	if records[1].Message.Type != loop.TypeUser || records[3].Message.Type != loop.TypeActivity {
		t.Errorf("message types = %q, %q", records[1].Message.Type, records[3].Message.Type)
	}

	activity := records[3].Message.Activity

	if activity == nil || activity.Kind != loop.ActivityResponse || activity.ID != "call_1" ||
		activity.Name != "shell" || activity.Arguments != `{"command":"go test ./..."}` || activity.Result != "ok" {
		t.Errorf("the call was not recorded whole: %+v", activity)
	}

	if event := records[2].Event; event.Kind != string(loop.EventToolCallStart) || event.Tool != "shell" || event.Iteration != 1 {
		t.Errorf("event = %+v", event)
	}

	result := records[4].Result

	if result.Reason != "settled" || result.Iterations != 3 || result.Settles != 1 || result.InputTokens != 1200 || result.OutputTokens != 340 {
		t.Errorf("result = %+v", result)
	}
}

// Token events are the same text the finished message already carries. Keeping
// them would multiply the size of every log for nothing.
func TestTokenNarrationIsNotRecorded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	writer, _ := Open(path, Meta{Task: "t"})

	recorder := NewRecorder(writer, nil)

	for _, kind := range []loop.EventKind{loop.EventToken, loop.EventReasoningToken} {
		recorder.Event(loop.Event{Kind: kind, Text: "hello", Iteration: 1})
	}

	recorder.Event(loop.Event{Kind: loop.EventIteration, Iteration: 1})

	_ = writer.Close()

	records := readLog(t, path)

	if got := kinds(records); fmt.Sprint(got) != fmt.Sprint([]Kind{KindMeta, KindEvent}) {
		t.Fatalf("records = %v, want the meta and the one real event", got)
	}

	if records[1].Event.Kind != "iteration" {
		t.Errorf("event = %+v", records[1].Event)
	}
}

// A log that stops taking lines is a run that has stopped being recorded, and the
// caller is told at once - once, however many lines follow - so it can end the run.
func TestARecorderReportsTheFirstFailedWrite(t *testing.T) {
	writer, err := Open(filepath.Join(t.TempDir(), "task.jsonl"), Meta{Task: "x"})
	if err != nil {
		t.Fatal(err)
	}

	var told []error

	recorder := NewRecorder(writer, func(err error) { told = append(told, err) })

	recorder.Event(loop.Event{Kind: loop.EventIteration})

	if recorder.Err() != nil || len(told) != 0 {
		t.Fatalf("a write that went through was reported: %v %v", recorder.Err(), told)
	}

	writer.Close()

	recorder.Conversation([]loop.Message{{Type: loop.TypeUser, Text: "a"}})
	recorder.Event(loop.Event{Kind: loop.EventIteration})
	recorder.Result(loop.Result{})

	if recorder.Err() == nil {
		t.Fatal("writes to a closed log were not reported")
	}

	if len(told) != 1 || told[0] != recorder.Err() {
		t.Errorf("the caller was told %d times (%v), want once, with the first failure", len(told), told)
	}
}

// Without anyone to tell, the failure is still kept.
func TestARecorderKeepsAFailureNobodyAskedAbout(t *testing.T) {
	writer, err := Open(filepath.Join(t.TempDir(), "task.jsonl"), Meta{Task: "x"})
	if err != nil {
		t.Fatal(err)
	}

	writer.Close()

	recorder := NewRecorder(writer, nil)
	recorder.Event(loop.Event{Kind: loop.EventIteration})

	if recorder.Err() == nil {
		t.Error("the failure was lost")
	}
}

func TestRecordResultKeepsTheUnderlyingError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	writer, err := Open(path, Meta{Task: "x"})
	if err != nil {
		t.Fatal(err)
	}

	recorder := NewRecorder(writer, nil)

	recorder.Result(loop.Result{
		Reason:  loop.StopError,
		Message: "the provider failed",
		Err: fmt.Errorf("provider: Model 'stealth/ox-alpha' not found (404): %w", &fantasy.ProviderError{
			StatusCode:   404,
			ResponseBody: []byte(`{"error":{"message":"not found"}}`),
			RequestBody:  make([]byte, 118234),
		}),
	})

	records := readLog(t, path)

	result := records[len(records)-1].Result

	if !strings.HasPrefix(result.Error, "provider: Model 'stealth/ox-alpha' not found (404)") {
		t.Errorf("Error = %q, want the provider's own words", result.Error)
	}

	failure := result.Failure
	if failure == nil || failure.Status != 404 || failure.RequestBytes != 118234 || !strings.Contains(failure.ResponseBody, "not found") {
		t.Errorf("Failure = %+v, want the wire evidence kept verbatim", failure)
	}
}

// The conversation only grows, and the recorder writes the tail it has not seen:
// called with the seed, then with the whole conversation again and again, each
// message lands once and in order.
func TestTheConversationIsRecordedOnceWhateverHowOftenItIsHandedOver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	writer, err := Open(path, Meta{Task: "x"})
	if err != nil {
		t.Fatal(err)
	}

	recorder := NewRecorder(writer, nil)

	conversation := []loop.Message{{Type: loop.TypeUser, Text: "the original task"}, {Type: loop.TypeUser, Text: "carry on"}}

	recorder.Conversation(conversation)
	recorder.Conversation(conversation)

	conversation = append(conversation, loop.Message{Type: loop.TypeBot, Text: "ok"})

	recorder.Conversation(conversation)

	recorder.Result(loop.Result{Reason: loop.StopSettled, Messages: conversation})

	var got []string

	for _, record := range readLog(t, path) {
		if record.Kind == KindMessage {
			got = append(got, record.Message.Text)
		}
	}

	if want := "the original task,carry on,ok"; strings.Join(got, ",") != want {
		t.Errorf("recorded %v, want each message once, in order: %s", got, want)
	}
}

// Usage numbers have dedicated fields the log has no column for: they are
// rendered into the event's text, or the log would say a usage event happened and
// nothing more.
func TestAUsageEventIsRecordedWithItsNumbers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	writer, _ := Open(path, Meta{Task: "t"})

	recorder := NewRecorder(writer, nil)

	recorder.Event(loop.Event{Kind: loop.EventUsage, InputTokens: 567000, OutputTokens: 1200, Iteration: 4})

	_ = writer.Close()

	events := readLog(t, path)

	if event := events[len(events)-1].Event; event == nil || event.Text != "input 567000 output 1200" {
		t.Errorf("event = %+v, want the token counts in its text", event)
	}
}

// The result carries the process exit code the ending maps onto, so a script
// reading the log needs no table of reasons.
func TestTheResultCarriesTheExitCode(t *testing.T) {
	for reason, want := range map[loop.StopReason]int{loop.StopSettled: 0, loop.StopFailed: 1, loop.StopAborted: 1} {
		path := filepath.Join(t.TempDir(), "task.jsonl")

		writer, _ := Open(path, Meta{Task: "t"})

		NewRecorder(writer, nil).Result(loop.Result{Reason: reason})

		records := readLog(t, path)

		if got := records[len(records)-1].Result.Code; got != want {
			t.Errorf("%s: code = %d, want %d", reason, got, want)
		}
	}
}

// The log's message records are loop's own types, and their JSON is the log's
// format: a change to a tag there would rewrite what every log says.
func TestAMessageRecordKeepsItsShapeOnDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	writer, _ := Open(path, Meta{Task: "t"})

	NewRecorder(writer, nil).Conversation([]loop.Message{{
		Type: loop.TypeActivity,
		Text: "ok",
		Activity: &loop.Activity{
			Kind: loop.ActivityResponse, ID: "c1", Name: "shell", Arguments: `{"command":"ls"}`, Result: "out",
		},
	}})

	_ = writer.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")

	var record struct {
		Message map[string]any `json:"message"`
	}

	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &record); err != nil {
		t.Fatal(err)
	}

	activity, _ := record.Message["activity"].(map[string]any)

	if record.Message["type"] != "activity" || record.Message["text"] != "ok" ||
		activity["kind"] != "response" || activity["id"] != "c1" || activity["name"] != "shell" ||
		activity["arguments"] != `{"command":"ls"}` || activity["result"] != "out" {
		t.Errorf("message record = %v, want type/text/activity{kind,id,name,arguments,result}", record.Message)
	}
}
