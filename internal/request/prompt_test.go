package request_test

import (
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/conversation"
	"github.com/openzot/openzot/internal/request"
	"github.com/openzot/openzot/internal/testutils"
)

func TestToPromptPairsToolCalls(t *testing.T) {
	messages := []conversation.Message{
		{Type: conversation.TypeUser, Text: "list the files"},
		testutils.Activity(conversation.ActivityRequest, "c1", "shell", `{"command":"ls"}`, nil),
		testutils.Activity(conversation.ActivityResponse, "c1", "shell", `{"command":"ls"}`, "README.md"),
		{Type: conversation.TypeBot, Text: "there is a README"},
	}

	prompt := request.ToPrompt(messages)

	require.Len(t, prompt, 4)

	assert.Equal(t, fantasy.MessageRoleUser, prompt[0].Role)

	call, ok := testutils.ToolCallOf(prompt[1])
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
		testutils.Activity(conversation.ActivityResponse, "c1", "shell", `{}`, "output"),
	}

	for _, message := range request.ToPrompt(messages) {
		require.NotEqual(t, fantasy.MessageRoleTool, message.Role, "an orphaned tool result must be dropped")
	}
}

// The mirror case. A request whose result never arrived leaves the conversation
// invalid, so the assistant turn goes too.
func TestToPromptDropsDanglingRequest(t *testing.T) {
	messages := []conversation.Message{
		{Type: conversation.TypeUser, Text: "go"},
		testutils.Activity(conversation.ActivityRequest, "c1", "shell", `{}`, nil),
	}

	prompt := request.ToPrompt(messages)

	for _, message := range prompt {
		_, ok := testutils.ToolCallOf(message)
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

	prompt := request.ToPrompt(messages)

	// reasoning is the model's scratchpad and providers reject their own
	// reasoning content on the way back in, so it is not replayed
	for _, message := range prompt {
		require.NotEqual(t, "thinking out loud", testutils.TextOf(message), "reasoning must not be replayed to the provider")
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
		testutils.Activity(conversation.ActivityRequest, "c1", "search", `{}`, nil),
		testutils.Activity(conversation.ActivityResponse, "c1", "search", `{}`, map[string]any{"records": []any{}}),
	}

	prompt := request.ToPrompt(messages)

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
		prompt := request.ToPrompt([]conversation.Message{message})
		assert.Empty(t, prompt, "case %d: a malformed activity reached the wire as %+v", index, prompt)
	}
}

// Forgetting takes the oldest message first, which is the opening user turn. A strict provider rejects a conversation with
// no user turn, so one is restored, and the model is pointed back at its instructions.
func TestBuildRestoresAUserTurnWhenForgettingTookIt(t *testing.T) {
	messages := []conversation.Message{
		testutils.Activity(conversation.ActivityRequest, "c1", "shell", `{"command":"ls"}`, nil),
		testutils.Activity(conversation.ActivityResponse, "c1", "shell", `{"command":"ls"}`, "README.md"),
	}

	prompt := request.Build(messages)

	require.Len(t, prompt, 3)

	assert.Equal(t, fantasy.MessageRoleUser, prompt[0].Role)
	assert.Equal(t, request.Kickoff, testutils.TextOf(prompt[0]))
}

// fantasy will not start a step from a conversation ending on the model's own words, so a handed-in one gets a line to
// continue from.
func TestBuildEndsOnAUserOrToolTurn(t *testing.T) {
	prompt := request.Build([]conversation.Message{
		{Type: conversation.TypeUser, Text: "go"},
		{Type: conversation.TypeBot, Text: "I began"},
	})

	require.Len(t, prompt, 3)

	assert.Equal(t, fantasy.MessageRoleUser, prompt[2].Role)
	assert.Equal(t, request.Kickoff, testutils.TextOf(prompt[2]))
}

// A conversation that already starts and ends the way a provider wants is sent as ToPrompt renders it.
func TestBuildLeavesAProperConversationAlone(t *testing.T) {
	messages := []conversation.Message{
		{Type: conversation.TypeUser, Text: "go"},
		testutils.Activity(conversation.ActivityRequest, "c1", "shell", `{}`, nil),
		testutils.Activity(conversation.ActivityResponse, "c1", "shell", `{}`, "ok"),
	}

	assert.Equal(t, request.ToPrompt(messages), request.Build(messages))
}
