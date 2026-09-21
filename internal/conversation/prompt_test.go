package conversation_test

import (
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/conversation"
)

// activity builds one half of a tool-call pair.
func activity(kind conversation.ActivityKind, name, arguments string, result any) conversation.Message {
	entry := &conversation.Activity{Kind: kind, ID: "c1", Name: name, Arguments: arguments}

	if kind == conversation.ActivityResponse {
		entry.Result = result
	}

	return conversation.Message{Type: conversation.TypeActivity, Text: entry.ResultText(), Activity: entry}
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

	var textSb35 strings.Builder

	for _, part := range message.Content {
		if piece, ok := part.(fantasy.TextPart); ok {
			textSb35.WriteString(piece.Text)
		}
	}

	text += textSb35.String()

	return text
}

func TestToPromptPairsToolCalls(t *testing.T) {
	messages := []conversation.Message{
		{Type: conversation.TypeUser, Text: "list the files"},
		activity(conversation.ActivityRequest, "shell", `{"command":"ls"}`, nil),
		activity(conversation.ActivityResponse, "shell", `{"command":"ls"}`, "README.md"),
		{Type: conversation.TypeBot, Text: "there is a README"},
	}

	prompt := conversation.ToPrompt(messages)

	require.Len(t, prompt, 4)

	assert.Equal(t, fantasy.MessageRoleUser, prompt[0].Role)

	call, ok := toolCallOf(prompt[1])
	require.Equal(t, fantasy.MessageRoleAssistant, prompt[1].Role, "prompt[1] should be an assistant turn carrying one tool call")
	require.True(t, ok, "prompt[1] should be an assistant turn carrying one tool call")

	assert.Equal(t, "c1", call.ToolCallID)
	assert.Equal(t, "shell", call.ToolName)
	assert.JSONEq(t, `{"command":"ls"}`, call.Input)

	require.Equal(t, fantasy.MessageRoleTool, prompt[2].Role)

	result, ok := prompt[2].Content[0].(fantasy.ToolResultPart)
	require.True(t, ok, "prompt[2] should reference c1: %+v", prompt[2])
	require.Equal(t, "c1", result.ToolCallID, "prompt[2] should reference c1: %+v", prompt[2])

	output, _ := result.Output.(fantasy.ToolResultOutputContentText)
	assert.Equal(t, "README.md", output.Text, "want the handler output")
}

// A tool result whose request was trimmed away would be rejected by the
// provider, so it must not be sent on its own.
func TestToPromptDropsOrphanedResult(t *testing.T) {
	messages := []conversation.Message{
		{Type: conversation.TypeUser, Text: "go"},
		activity(conversation.ActivityResponse, "shell", `{}`, "output"),
	}

	for _, message := range conversation.ToPrompt(messages) {
		require.NotEqual(t, fantasy.MessageRoleTool, message.Role, "an orphaned tool result must be dropped")
	}
}

// The mirror case. A request whose result never arrived leaves the conversation
// invalid, so the assistant turn goes too.
func TestToPromptDropsDanglingRequest(t *testing.T) {
	messages := []conversation.Message{
		{Type: conversation.TypeUser, Text: "go"},
		activity(conversation.ActivityRequest, "shell", `{}`, nil),
	}

	prompt := conversation.ToPrompt(messages)

	for _, message := range prompt {
		_, ok := toolCallOf(message)
		require.False(t, ok, "a request with no result must be dropped: %+v", message)
	}

	assert.Len(t, prompt, 1, "want just the user turn")
}

func TestToPromptRoleMapping(t *testing.T) {
	messages := []conversation.Message{
		{Type: conversation.TypeInstructions, Text: "you are an agent"},
		{Type: conversation.TypeReasoning, Text: "thinking out loud"},
		{Type: conversation.TypeBot, Text: "the answer"},
		{Type: conversation.TypeUser, Text: "a question"},
	}

	prompt := conversation.ToPrompt(messages)

	// reasoning is the model's scratchpad and providers reject their own
	// reasoning content on the way back in, so it is not replayed
	for _, message := range prompt {
		require.NotEqual(t, "thinking out loud", textOf(message), "reasoning must not be replayed to the provider")
	}

	want := []fantasy.MessageRole{
		fantasy.MessageRoleSystem,
		fantasy.MessageRoleAssistant,
		fantasy.MessageRoleUser,
	}

	require.Len(t, prompt, len(want))

	for index, role := range want {
		assert.Equal(t, role, prompt[index].Role)
	}
}

func TestToPromptEncodesStructuredResults(t *testing.T) {
	messages := []conversation.Message{
		activity(conversation.ActivityRequest, "search", `{}`, nil),
		activity(conversation.ActivityResponse, "search", `{}`, map[string]any{"records": []any{}}),
	}

	prompt := conversation.ToPrompt(messages)

	require.Len(t, prompt, 2)

	result, _ := prompt[1].Content[0].(fantasy.ToolResultPart)

	output, _ := result.Output.(fantasy.ToolResultOutputContentText)
	assert.JSONEq(t, `{"records":[]}`, output.Text, "want it JSON-encoded")
}

func TestMalformedActivitiesDoNotReachTheWire(t *testing.T) {
	cases := []conversation.Message{
		{Type: conversation.TypeActivity},
		{Type: conversation.TypeActivity, Activity: &conversation.Activity{}},
		{Type: conversation.TypeActivity, Activity: &conversation.Activity{Kind: "somethingelse", ID: "c1"}},
	}

	for index, message := range cases {
		prompt := conversation.ToPrompt([]conversation.Message{message})
		assert.Empty(t, prompt, "case %d: a malformed activity reached the wire as %+v", index, prompt)
	}
}
