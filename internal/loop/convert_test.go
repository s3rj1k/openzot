package loop

import (
	"testing"

	"charm.land/fantasy"
)

// activity builds one half of a tool-call pair.
func activity(kind ActivityKind, id, name, arguments string, result any) Message {
	entry := &Activity{Kind: kind, ID: id, Name: name, Arguments: arguments}

	if kind == ActivityResponse {
		entry.Result = result
	}

	return Message{Type: TypeActivity, Text: entry.ResultText(), Activity: entry}
}

// toolCallOf returns the call an assistant message carries, if it carries one.
func toolCallOf(message fantasy.Message) (fantasy.ToolCallPart, bool) {
	for _, part := range message.Content {
		if call, ok := part.(fantasy.ToolCallPart); ok {
			return call, true
		}
	}

	return fantasy.ToolCallPart{}, false
}

// textOf joins the text parts of a message.
func textOf(message fantasy.Message) string {
	var text string

	for _, part := range message.Content {
		if piece, ok := part.(fantasy.TextPart); ok {
			text += piece.Text
		}
	}

	return text
}

func TestToPromptPairsToolCalls(t *testing.T) {
	messages := []Message{
		{Type: TypeUser, Text: "list the files"},
		activity(ActivityRequest, "c1", "shell", `{"command":"ls"}`, nil),
		activity(ActivityResponse, "c1", "shell", `{"command":"ls"}`, "README.md"),
		{Type: TypeBot, Text: "there is a README"},
	}

	prompt := toPrompt(messages)

	if len(prompt) != 4 {
		t.Fatalf("got %d messages, want 4: %+v", len(prompt), prompt)
	}

	if prompt[0].Role != fantasy.MessageRoleUser {
		t.Errorf("prompt[0] role = %q, want user", prompt[0].Role)
	}

	call, ok := toolCallOf(prompt[1])
	if prompt[1].Role != fantasy.MessageRoleAssistant || !ok {
		t.Fatalf("prompt[1] should be an assistant turn carrying one tool call: %+v", prompt[1])
	}

	if call.ToolCallID != "c1" || call.ToolName != "shell" || call.Input != `{"command":"ls"}` {
		t.Errorf("tool call = %+v", call)
	}

	if prompt[2].Role != fantasy.MessageRoleTool {
		t.Fatalf("prompt[2] should be a tool result: %+v", prompt[2])
	}

	result, ok := prompt[2].Content[0].(fantasy.ToolResultPart)
	if !ok || result.ToolCallID != "c1" {
		t.Fatalf("prompt[2] should reference c1: %+v", prompt[2])
	}

	if output, _ := result.Output.(fantasy.ToolResultOutputContentText); output.Text != "README.md" {
		t.Errorf("tool result = %+v, want the handler output", result.Output)
	}
}

// A tool result whose request was trimmed away would be rejected by the
// provider, so it must not be sent on its own.
func TestToPromptDropsOrphanedResult(t *testing.T) {
	messages := []Message{
		{Type: TypeUser, Text: "go"},
		activity(ActivityResponse, "c1", "shell", `{}`, "output"),
	}

	for _, message := range toPrompt(messages) {
		if message.Role == fantasy.MessageRoleTool {
			t.Fatalf("an orphaned tool result must be dropped: %+v", message)
		}
	}
}

// The mirror case: a request whose result never arrived leaves the conversation
// invalid, so the assistant turn goes too.
func TestToPromptDropsDanglingRequest(t *testing.T) {
	messages := []Message{
		{Type: TypeUser, Text: "go"},
		activity(ActivityRequest, "c1", "shell", `{}`, nil),
	}

	prompt := toPrompt(messages)

	for _, message := range prompt {
		if _, ok := toolCallOf(message); ok {
			t.Fatalf("a request with no result must be dropped: %+v", message)
		}
	}

	if len(prompt) != 1 {
		t.Errorf("got %d messages, want just the user turn", len(prompt))
	}
}

func TestToPromptRoleMapping(t *testing.T) {
	messages := []Message{
		{Type: TypeInstructions, Text: "you are an agent"},
		{Type: TypeReasoning, Text: "thinking out loud"},
		{Type: TypeBot, Text: "the answer"},
		{Type: TypeUser, Text: "a question"},
	}

	prompt := toPrompt(messages)

	// reasoning is the model's scratchpad and providers reject their own
	// reasoning content on the way back in, so it is not replayed
	for _, message := range prompt {
		if textOf(message) == "thinking out loud" {
			t.Fatal("reasoning must not be replayed to the provider")
		}
	}

	want := []fantasy.MessageRole{
		fantasy.MessageRoleSystem,
		fantasy.MessageRoleAssistant,
		fantasy.MessageRoleUser,
	}

	if len(prompt) != len(want) {
		t.Fatalf("got %d messages, want %d: %+v", len(prompt), len(want), prompt)
	}

	for index, role := range want {
		if prompt[index].Role != role {
			t.Errorf("prompt[%d] role = %q, want %q", index, prompt[index].Role, role)
		}
	}
}

func TestToPromptEncodesStructuredResults(t *testing.T) {
	messages := []Message{
		activity(ActivityRequest, "c1", "search", `{}`, nil),
		activity(ActivityResponse, "c1", "search", `{}`, map[string]any{"records": []any{}}),
	}

	prompt := toPrompt(messages)

	if len(prompt) != 2 {
		t.Fatalf("got %d messages, want 2", len(prompt))
	}

	result, _ := prompt[1].Content[0].(fantasy.ToolResultPart)

	if output, _ := result.Output.(fantasy.ToolResultOutputContentText); output.Text != `{"records":[]}` {
		t.Errorf("structured result = %+v, want it JSON-encoded", result.Output)
	}
}

func TestMalformedActivitiesDoNotReachTheWire(t *testing.T) {
	cases := []Message{
		{Type: TypeActivity},
		{Type: TypeActivity, Activity: &Activity{}},
		{Type: TypeActivity, Activity: &Activity{Kind: "somethingelse", ID: "c1"}},
	}

	for index, message := range cases {
		if prompt := toPrompt([]Message{message}); len(prompt) != 0 {
			t.Errorf("case %d: a malformed activity reached the wire as %+v", index, prompt)
		}
	}
}
