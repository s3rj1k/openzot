package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/conversation"
	"github.com/openzot/openzot/internal/loop"
	"github.com/openzot/openzot/internal/session"
	"github.com/openzot/openzot/internal/testutils"
)

// The model's reasoning is part of the record. The scratchpad is often the only
// place that says why it did what it did. It arrives as its own message, kept in
// order between what the user said and what the model answered.
func TestTheModelsReasoningIsRecordedInOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	writer, err := session.Open(path, session.Meta{Task: litAddAHealthEndpoint})
	require.NoError(t, err)

	recorder := newRecorder(writer, nil)

	recorder.Conversation([]conversation.Message{
		{Type: conversation.TypeUser, Text: litAddAHealthEndpoint},
		{Type: conversation.TypeReasoning, Text: "I should look at the router first,\nthen add the handler."},
		{Type: conversation.TypeBot, Text: litOnIt},
	})

	records := testutils.ReadLog(t, path)

	got := make([]string, 0, len(records)-1)

	for _, record := range records[1:] {
		got = append(got, string(record.Message.Type)+": "+record.Message.Text)
	}

	want := []string{
		"user: add a health endpoint",
		"reasoning: I should look at the router first,\nthen add the handler.",
		"bot: on it",
	}

	assert.Equal(t, fmt.Sprint(want), fmt.Sprint(got))
}

func TestARunIsRecordedFromItsFirstMessageToItsOutcome(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	writer, err := session.Open(path, session.Meta{Task: litAddAHealthEndpoint})
	require.NoError(t, err)

	recorder := newRecorder(writer, nil)

	messages := make([]conversation.Message, 0, 2)
	messages = append(messages, conversation.Message{Type: conversation.TypeUser, Text: litAddAHealthEndpoint})

	recorder.Conversation(messages)

	recorder.Event(loop.Event{Kind: loop.EventToolCallStart, Tool: litShell, Text: "go test ./...", Iteration: 1})

	messages = append(messages, conversation.Message{
		Type: conversation.TypeActivity,
		Text: "ok",
		Activity: &conversation.Activity{
			Kind:      conversation.ActivityResponse,
			ID:        litCall1,
			Name:      litShell,
			Arguments: litCommandGoTest,
			Result:    "ok",
		},
	})

	recorder.Result(&loop.Result{
		Reason:   loop.StopSettled,
		Message:  "finished",
		Messages: messages,
		Budget:   loop.Budget{Iterations: 3, Calls: 2, Cycles: 1, Settles: 1, InputTokens: 1200, OutputTokens: 340},
	})

	records := testutils.ReadLog(t, path)

	got := testutils.Kinds(records)
	want := []session.Kind{session.KindMeta, session.KindMessage, session.KindEvent, session.KindMessage, session.KindResult}

	require.Equal(t, fmt.Sprint(want), fmt.Sprint(got))

	// the type has to survive as the string the engine's own type names
	assert.Equal(t, conversation.TypeUser, records[1].Message.Type)
	assert.Equal(t, conversation.TypeActivity, records[3].Message.Type)

	activity := records[3].Message.Activity

	assert.NotNil(t, activity, "the call was not recorded whole")
	assert.Equal(t, conversation.ActivityResponse, activity.Kind, "the call was not recorded whole")
	assert.Equal(t, litCall1, activity.ID, "the call was not recorded whole")
	assert.Equal(t, litShell, activity.Name, "the call was not recorded whole")
	assert.JSONEq(t, litCommandGoTest, activity.Arguments, "the call was not recorded whole")
	assert.Equal(t, "ok", activity.Result, "the call was not recorded whole")

	event := records[2].Event
	assert.Equal(t, string(loop.EventToolCallStart), event.Kind)
	assert.Equal(t, litShell, event.Tool)
	assert.Equal(t, 1, event.Iteration)

	result := records[4].Result

	assert.Equal(t, litSettled, result.Reason)
	assert.Equal(t, 3, result.Iterations)
	assert.Equal(t, 1, result.Settles)
	assert.Equal(t, 1200, result.InputTokens)
	assert.Equal(t, 340, result.OutputTokens)
}

// Token events are the same text the finished message already carries. Keeping
// them would multiply the size of every log for nothing.
func TestTokenNarrationIsNotRecorded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	writer, _ := session.Open(path, session.Meta{Task: "t"})

	recorder := newRecorder(writer, nil)

	for _, kind := range []loop.EventKind{loop.EventToken, loop.EventReasoningToken} {
		recorder.Event(loop.Event{Kind: kind, Text: "hello", Iteration: 1})
	}

	recorder.Event(loop.Event{Kind: loop.EventIteration, Iteration: 1})

	_ = writer.Close()

	records := testutils.ReadLog(t, path)

	require.Equal(t, fmt.Sprint([]session.Kind{session.KindMeta, session.KindEvent}), fmt.Sprint(testutils.Kinds(records)), "want the meta and the one real event")

	assert.Equal(t, "iteration", records[1].Event.Kind)
}

// A log that stops taking lines is a run that has stopped being recorded, and the
// caller is told at once - once, however many lines follow - so it can end the run.
func TestARecorderReportsTheFirstFailedWrite(t *testing.T) {
	writer, err := session.Open(filepath.Join(t.TempDir(), "task.jsonl"), session.Meta{Task: "x"})
	require.NoError(t, err)

	var told []error

	recorder := newRecorder(writer, func(err error) { told = append(told, err) })

	recorder.Event(loop.Event{Kind: loop.EventIteration})

	require.NoError(t, recorder.Err(), "a write that went through was reported")
	require.Empty(t, told, "a write that went through was reported")

	writer.Close()

	recorder.Conversation([]conversation.Message{{Type: conversation.TypeUser, Text: "a"}})
	recorder.Event(loop.Event{Kind: loop.EventIteration})
	recorder.Result(&loop.Result{})

	require.Error(t, recorder.Err(), "writes to a closed log were not reported")

	assert.Len(t, told, 1, "want once, with the first failure")
	require.ErrorIs(t, told[0], recorder.Err(), "want once, with the first failure")
}

// Without anyone to tell, the failure is still kept.
func TestARecorderKeepsAFailureNobodyAskedAbout(t *testing.T) {
	writer, err := session.Open(filepath.Join(t.TempDir(), "task.jsonl"), session.Meta{Task: "x"})
	require.NoError(t, err)

	writer.Close()

	recorder := newRecorder(writer, nil)
	recorder.Event(loop.Event{Kind: loop.EventIteration})

	require.Error(t, recorder.Err())
}

func TestRecordResultKeepsTheUnderlyingError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	writer, err := session.Open(path, session.Meta{Task: "x"})
	require.NoError(t, err)

	recorder := newRecorder(writer, nil)

	recorder.Result(&loop.Result{
		Reason:  loop.StopError,
		Message: "the provider failed",
		Err: fmt.Errorf("provider: Model 'stealth/ox-alpha' not found (404): %w", &fantasy.ProviderError{
			StatusCode:   404,
			ResponseBody: []byte(`{"error":{"message":"not found"}}`),
			RequestBody:  make([]byte, 118234),
		}),
	})

	records := testutils.ReadLog(t, path)

	result := records[len(records)-1].Result

	assert.True(t, strings.HasPrefix(result.Error, "provider: Model 'stealth/ox-alpha' not found (404)"), "want the provider's own words")

	failure := result.Failure
	assert.NotNil(t, failure, "want the wire evidence kept verbatim")
	assert.Equal(t, 404, failure.Status, "want the wire evidence kept verbatim")
	assert.Equal(t, 118234, failure.RequestBytes, "want the wire evidence kept verbatim")
	assert.Contains(t, failure.ResponseBody, "not found", "want the wire evidence kept verbatim")
}

// The conversation only grows, and the recorder writes the tail it has not seen.
// Called with the seed, then with the whole conversation again and again, each
// message lands once and in order.
func TestTheConversationIsRecordedOnceWhateverHowOftenItIsHandedOver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	writer, err := session.Open(path, session.Meta{Task: "x"})
	require.NoError(t, err)

	recorder := newRecorder(writer, nil)

	messages := make([]conversation.Message, 0, 3)
	messages = append(messages,
		conversation.Message{Type: conversation.TypeUser, Text: "the original task"},
		conversation.Message{Type: conversation.TypeUser, Text: "carry on"},
	)

	recorder.Conversation(messages)
	recorder.Conversation(messages)

	messages = append(messages, conversation.Message{Type: conversation.TypeBot, Text: "ok"})

	recorder.Conversation(messages)

	recorder.Result(&loop.Result{Reason: loop.StopSettled, Messages: messages})

	var got []string

	for _, record := range testutils.ReadLog(t, path) {
		if record.Kind == session.KindMessage {
			got = append(got, record.Message.Text)
		}
	}

	want := "the original task,carry on,ok"
	assert.Equal(t, want, strings.Join(got, ","), "recorded %v, want each message once, in order", got)
}

// Usage numbers have dedicated fields the log has no column for. They are
// rendered into the event's text, or the log would say a usage event happened and
// nothing more.
func TestAUsageEventIsRecordedWithItsNumbers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	writer, _ := session.Open(path, session.Meta{Task: "t"})

	recorder := newRecorder(writer, nil)

	recorder.Event(loop.Event{Kind: loop.EventUsage, InputTokens: 567000, OutputTokens: 1200, Iteration: 4})

	_ = writer.Close()

	events := testutils.ReadLog(t, path)

	event := events[len(events)-1].Event
	assert.NotNil(t, event, "want the token counts in its text")
	assert.Equal(t, "input 567000 output 1200", event.Text, "want the token counts in its text")
}

// The result carries the process exit code the ending maps onto, so a script
// reading the log needs no table of reasons.
func TestTheResultCarriesTheExitCode(t *testing.T) {
	for reason, want := range map[loop.StopReason]int{loop.StopSettled: 0, loop.StopFailed: 1, loop.StopAborted: 1} {
		path := filepath.Join(t.TempDir(), "task.jsonl")

		writer, _ := session.Open(path, session.Meta{Task: "t"})

		newRecorder(writer, nil).Result(&loop.Result{Reason: reason})

		records := testutils.ReadLog(t, path)

		assert.Equal(t, want, records[len(records)-1].Result.Code, "%s: code", reason)
	}
}

// The log's message records are loop's own types, and their JSON is the log's
// format. A change to a tag there would rewrite what every log says.
func TestAMessageRecordKeepsItsShapeOnDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "task.jsonl")

	writer, _ := session.Open(path, session.Meta{Task: "t"})

	newRecorder(writer, nil).Conversation([]conversation.Message{{
		Type: conversation.TypeActivity,
		Text: "ok",
		Activity: &conversation.Activity{
			Kind: conversation.ActivityResponse, ID: "c1", Name: litShell, Arguments: litCommandLs, Result: "out",
		},
	}})

	_ = writer.Close()

	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")

	var record struct {
		Message map[string]any `json:"message"`
	}

	require.NoError(t, json.Unmarshal([]byte(lines[len(lines)-1]), &record))

	activity, _ := record.Message[litActivity].(map[string]any)

	assert.Equal(t, litActivity, record.Message["type"], "want type/text/activity{kind,id,name,arguments,result}")
	assert.Equal(t, "ok", record.Message["text"], "want type/text/activity{kind,id,name,arguments,result}")
	assert.Equal(t, "response", activity["kind"], "want type/text/activity{kind,id,name,arguments,result}")
	assert.Equal(t, "c1", activity["id"], "want type/text/activity{kind,id,name,arguments,result}")
	assert.Equal(t, litShell, activity["name"], "want type/text/activity{kind,id,name,arguments,result}")
	arguments, _ := activity["arguments"].(string)

	assert.JSONEq(t, litCommandLs, arguments, "want type/text/activity{kind,id,name,arguments,result}")
	assert.Equal(t, "out", activity["result"], "want type/text/activity{kind,id,name,arguments,result}")
}
