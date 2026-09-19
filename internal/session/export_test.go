package session

import (
	"encoding/json"
	"testing"
)

// A session exported for training reads as one chat conversation: the
// reasoning, the answer and the tool calls zot records as separate turns fold
// into one assistant message, each tool result is a tool turn that names the
// call it answers, and the facts about the run travel beside the conversation.
func TestExportFoldsATurnIntoTheChatShape(t *testing.T) {
	dir := t.TempDir()

	writer, err := Create(dir, "20260822-100000", Meta{Task: "make a game", Model: "m", Provider: "p", Driver: "d"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	must := func(err error) {
		t.Helper()

		if err != nil {
			t.Fatal(err)
		}
	}

	must(writer.Message(Message{Type: "user", Text: "Begin working on your task."}))
	must(writer.Event(Event{Kind: "iteration", Iteration: 1}))
	must(writer.Message(Message{Type: "reasoning", Text: "I should look around first."}))
	must(writer.Message(Message{Type: "bot", Text: "Looking around."}))
	must(writer.Message(Message{Type: "activity", Activity: &Activity{Kind: "request", ID: "call-1", Name: "list", Arguments: `{"path":"."}`}}))
	must(writer.Message(Message{Type: "activity", Activity: &Activity{Kind: "request", ID: "call-2", Name: "read", Arguments: `{"path":"README.md"}`}}))
	must(writer.Message(Message{Type: "activity", Activity: &Activity{Kind: "response", ID: "call-1", Name: "list", Result: "README.md\n"}}))
	must(writer.Message(Message{Type: "activity", Activity: &Activity{Kind: "response", ID: "call-2", Name: "read", Failure: "read: no such file"}}))
	must(writer.Event(Event{Kind: "iteration", Iteration: 2}))
	must(writer.Message(Message{Type: "bot", Text: "Done."}))
	must(writer.Result(Result{Reason: "success", Iterations: 2, Calls: 2}))

	loaded, err := Load(writer.Path())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	trajectory, err := Export(loaded)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	if trajectory.ID != "20260822-100000" || trajectory.Task != "make a game" || trajectory.Model != "m" {
		t.Errorf("trajectory header = %+v", trajectory)
	}

	if !trajectory.Complete || trajectory.Outcome == nil || trajectory.Outcome.Reason != "success" {
		t.Errorf("outcome = %+v (complete %v)", trajectory.Outcome, trajectory.Complete)
	}

	if trajectory.Events["iteration"] != 2 {
		t.Errorf("events = %v, want 2 iterations", trajectory.Events)
	}

	if trajectory.Started.IsZero() || trajectory.Ended.Before(trajectory.Started) {
		t.Errorf("started %v, ended %v", trajectory.Started, trajectory.Ended)
	}

	roles := []string{}
	for _, message := range trajectory.Messages {
		roles = append(roles, message.Role)
	}

	want := []string{"user", "assistant", "tool", "tool", "assistant"}
	if len(roles) != len(want) {
		t.Fatalf("roles = %v, want %v", roles, want)
	}

	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("roles = %v, want %v", roles, want)
		}
	}

	turn := trajectory.Messages[1]

	if turn.Reasoning != "I should look around first." || turn.Content != "Looking around." {
		t.Errorf("assistant turn = %+v", turn)
	}

	if len(turn.ToolCalls) != 2 || turn.ToolCalls[0].Function.Name != "list" || turn.ToolCalls[1].ID != "call-2" {
		t.Errorf("tool calls = %+v", turn.ToolCalls)
	}

	if turn.ToolCalls[0].Type != "function" || turn.ToolCalls[0].Function.Arguments != `{"path":"."}` {
		t.Errorf("tool call shape = %+v", turn.ToolCalls[0])
	}

	result := trajectory.Messages[2]

	if result.ToolCallID != "call-1" || result.Name != "list" || result.Content != "README.md\n" {
		t.Errorf("tool result = %+v", result)
	}

	if failed := trajectory.Messages[3]; failed.Content != "read: no such file" {
		t.Errorf("failed tool result = %+v", failed)
	}

	// the shape has to survive the wire: encode and make sure the OpenAI
	// field names are the ones written
	encoded, err := json.Marshal(trajectory)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	messages := decoded["messages"].([]any)
	assistant := messages[1].(map[string]any)

	if _, ok := assistant["tool_calls"]; !ok {
		t.Errorf("assistant turn lacks tool_calls: %v", assistant)
	}

	if tool := messages[2].(map[string]any); tool["tool_call_id"] != "call-1" {
		t.Errorf("tool turn lacks tool_call_id: %v", tool)
	}
}

func TestExportRendersAStructuredToolResult(t *testing.T) {
	session := &Session{Meta: Meta{ID: "x"}, Messages: []Message{
		{Type: "activity", Activity: &Activity{
			Kind: "request", ID: "c1", Name: "shell", Arguments: `{"command":"ls"}`,
		}},
		{Type: "activity", Activity: &Activity{Kind: "response", ID: "c1", Name: "shell", Result: map[string]any{"stdout": "a\n"}}},
	}}

	trajectory, err := Export(session)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	// a structured tool result is rendered as JSON, the way the model read it
	if trajectory.Messages[1].Content != `{"stdout":"a\n"}` {
		t.Errorf("structured result = %q", trajectory.Messages[1].Content)
	}

	if trajectory.Complete {
		t.Error("a session without a result must not export as complete")
	}
}
