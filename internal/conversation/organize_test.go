package conversation_test

import (
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/conversation"
	"github.com/openzot/openzot/internal/testutils"
)

// These cases are the shapes that reached production - a result whose call was trimmed away, two matching
// calls in one turn, a trigger stranded mid-history - each a request a provider rejects.

// kinds renders a conversation as a comparable shape.
func kinds(messages []conversation.Message) []string {
	var out []string

	for _, message := range messages {
		if activity := message.Activity; activity != nil {
			out = append(out, string(message.Type)+"/"+string(activity.Kind)+"/"+activity.ID)

			continue
		}

		out = append(out, string(message.Type)+"/"+message.Text)
	}

	return out
}

func TestOrganizeKeepsAWellFormedConversation(t *testing.T) {
	messages := []conversation.Message{
		{Type: conversation.TypeInstructions, Text: litYouAreACoding},
		{Type: conversation.TypeUser, Text: "run the tests"},
		testutils.Request("call_1", "shell", `{"command":"go test"}`),
		testutils.Response("call_1", "shell", `{"command":"go test"}`, "ok"),
		{Type: conversation.TypeBot, Text: "they pass"},
	}

	got := conversation.Organize(messages)

	assert.Equal(t, kinds(messages), kinds(got), "a valid conversation must survive untouched")
}

// The rule providers enforce. A result immediately follows the call it answers.
func TestOrganizeClustersASeparatedPair(t *testing.T) {
	got := conversation.Organize([]conversation.Message{
		testutils.Request("call_1", "shell", "{}"),
		{Type: conversation.TypeBot, Text: "thinking about it"},
		testutils.Response("call_1", "shell", "{}", "ok"),
	})

	want := []string{litActivityRequestCall1, litActivityResponseCall1, "bot/thinking about it"}

	assert.Equal(t, want, kinds(got))
}

func TestOrganizeClustersInterleavedPairs(t *testing.T) {
	got := conversation.Organize([]conversation.Message{
		testutils.Request("call_1", "read", `{"path":"a"}`),
		testutils.Request("call_2", "read", `{"path":"b"}`),
		testutils.Response("call_2", "read", `{"path":"b"}`, "b contents"),
		testutils.Response("call_1", "read", `{"path":"a"}`, "a contents"),
	})

	want := []string{
		litActivityRequestCall1,
		litActivityResponseCall1,
		"activity/request/call_2",
		"activity/response/call_2",
	}

	assert.Equal(t, want, kinds(got), "interleaved pairs were not reunited")
}

// Trimming can take either end of a pair, and both leave a request a provider
// will not accept.
func TestOrganizeDropsOrphans(t *testing.T) {
	tests := []struct {
		name     string
		messages []conversation.Message
		want     []string
	}{
		{
			name: "a call whose result was trimmed away",
			messages: []conversation.Message{
				{Type: conversation.TypeUser, Text: "go"},
				testutils.Request("call_1", "shell", "{}"),
			},
			want: []string{litUserGo},
		},
		{
			name: "a result whose call was trimmed away",
			messages: []conversation.Message{
				testutils.Response("call_1", "shell", "{}", "ok"),
				{Type: conversation.TypeBot, Text: "done"},
			},
			want: []string{"bot/done"},
		},
		{
			name: "one good pair and one orphan",
			messages: []conversation.Message{
				testutils.Request("call_1", "shell", "{}"),
				testutils.Response("call_1", "shell", "{}", "ok"),
				testutils.Request("call_2", "shell", "{}"),
			},
			want: []string{litActivityRequestCall1, litActivityResponseCall1},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, kinds(conversation.Organize(test.messages)))
		})
	}
}

// Two matching calls in one turn are only distinguishable by id, which is why
// the pairing prefers it over the arguments.
func TestOrganizePairsIdenticalCallsByID(t *testing.T) {
	got := conversation.Organize([]conversation.Message{
		testutils.Request("call_1", "read", `{"path":"a"}`),
		testutils.Request("call_2", "read", `{"path":"a"}`),
		testutils.Response("call_2", "read", `{"path":"a"}`, "second"),
		testutils.Response("call_1", "read", `{"path":"a"}`, "first"),
	})

	require.Len(t, got, 4, "both calls must keep their own result")

	assert.Equal(t, "first", got[1].Text, "results were paired with the wrong calls: %q then %q", got[1].Text, got[3].Text)
	assert.Equal(t, "second", got[3].Text, "results were paired with the wrong calls: %q then %q", got[1].Text, got[3].Text)
}

// A history rebuilt from somewhere that did not keep call ids still has to
// pair up, which is what the TypeScript engine matched on.
func TestOrganizePairsWithoutIDs(t *testing.T) {
	got := conversation.Organize([]conversation.Message{
		testutils.Request("", "shell", `{"command":"ls"}`),
		{Type: conversation.TypeReasoning, Text: "let me look"},
		testutils.Response("", "shell", `{"command":"ls"}`, "a\nb"),
	})

	want := []string{"activity/request/", "activity/response/", "reasoning/let me look"}

	assert.Equal(t, want, kinds(got))
}

func TestOrganizeDoesNotPairDifferentCalls(t *testing.T) {
	got := conversation.Organize([]conversation.Message{
		testutils.Request("", "read", `{"path":"a"}`),
		testutils.Response("", "read", `{"path":"b"}`, "b contents"),
	})

	assert.Empty(t, got, "a result for a different call is not a partner")
}

// Two calls cannot pair with each other, nor two results.
func TestOrganizeRequiresOneOfEach(t *testing.T) {
	got := conversation.Organize([]conversation.Message{
		testutils.Request("call_1", "shell", "{}"),
		testutils.Request("call_1", "shell", "{}"),
	})

	assert.Empty(t, got, "two calls do not make a pair")

	got = conversation.Organize([]conversation.Message{
		testutils.Response("call_1", "shell", "{}", "ok"),
		testutils.Response("call_1", "shell", "{}", "ok"),
	})

	assert.Empty(t, got, "two results do not make a pair")
}

// A trigger says "act now". Anywhere but last it describes a moment that has
// already passed.
func TestOrganizeKeepsATriggerOnlyWhenItIsLast(t *testing.T) {
	got := conversation.Organize([]conversation.Message{
		{Type: conversation.TypeUser, Text: "go"},
		testutils.Trigger("wake"),
	})

	assert.Equal(t, []string{litUserGo, "activity/trigger/"}, kinds(got))

	got = conversation.Organize([]conversation.Message{
		testutils.Trigger("wake"),
		{Type: conversation.TypeUser, Text: "go"},
	})

	assert.Equal(t, []string{litUserGo}, kinds(got), "a stranded trigger must be dropped")
}

// The system prompt is bookkeeping rather than conversation, so a trigger
// followed only by instructions is still last.
func TestOrganizeTriggerIgnoresTrailingInstructions(t *testing.T) {
	got := conversation.Organize([]conversation.Message{
		{Type: conversation.TypeUser, Text: "go"},
		testutils.Trigger("wake"),
		{Type: conversation.TypeInstructions, Text: litYouAreACoding},
	})

	want := []string{litUserGo, "activity/trigger/", "instructions/you are a coding agent"}

	assert.Equal(t, want, kinds(got))
}

// An activity message with nothing in its meta describes no call at all - it
// cannot be paired, rendered or acted on.
func TestOrganizeDropsMalformedActivities(t *testing.T) {
	tests := []struct {
		name    string
		message conversation.Message
	}{
		{name: "no activity at all", message: conversation.Message{Type: conversation.TypeActivity}},
		{name: "an empty activity", message: conversation.Message{Type: conversation.TypeActivity, Activity: &conversation.Activity{}}},
		{
			name:    "no kind",
			message: conversation.Message{Type: conversation.TypeActivity, Activity: &conversation.Activity{ID: "x", Name: "shell"}},
		},
		{
			name:    "a kind nobody recognizes",
			message: conversation.Message{Type: conversation.TypeActivity, Activity: &conversation.Activity{Kind: "somethingelse", ID: "x"}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := conversation.Organize([]conversation.Message{{Type: conversation.TypeUser, Text: "go"}, test.message})

			assert.Equal(t, []string{litUserGo}, kinds(got))
		})
	}
}

func TestOrganizeDropsEmptyMessages(t *testing.T) {
	got := conversation.Organize([]conversation.Message{
		{Type: conversation.TypeUser, Text: "go"},
		{Type: conversation.TypeBot, Text: ""},
		{Type: conversation.TypeReasoning, Text: ""},
		{Type: conversation.TypeBot, Text: "done"},
	})

	want := []string{litUserGo, "bot/done"}

	assert.Equal(t, want, kinds(got))
}

// An empty system prompt is a configuration choice rather than an accident, and
// dropping it would change which message comes first.
func TestOrganizeKeepsAnEmptyInstructions(t *testing.T) {
	got := conversation.Organize([]conversation.Message{{Type: conversation.TypeInstructions, Text: ""}, {Type: conversation.TypeUser, Text: "go"}})

	assert.Len(t, got, 2)
	assert.Equal(t, conversation.TypeInstructions, got[0].Type)
}

// A retried turn or a re-injected notice can land twice. Repetition is also
// what the model imitates.
func TestOrganizeCollapsesConsecutiveDuplicates(t *testing.T) {
	got := conversation.Organize([]conversation.Message{
		{Type: conversation.TypeUser, Text: "go"},
		{Type: conversation.TypeUser, Text: "go"},
		{Type: conversation.TypeBot, Text: "ok"},
		{Type: conversation.TypeUser, Text: "go"},
	})

	want := []string{litUserGo, "bot/ok", litUserGo}

	assert.Equal(t, want, kinds(got), "only consecutive repeats collapse")
}

// Two calls with the same arguments are two real calls the model made.
// Collapsing them would hide the repetition the cycle guards exist to catch.
func TestOrganizeDoesNotCollapseRepeatedToolCalls(t *testing.T) {
	got := conversation.Organize([]conversation.Message{
		testutils.Request("call_1", "read", `{"path":"a"}`),
		testutils.Response("call_1", "read", `{"path":"a"}`, "contents"),
		testutils.Request("call_2", "read", `{"path":"a"}`),
		testutils.Response("call_2", "read", `{"path":"a"}`, "contents"),
	})

	assert.Len(t, got, 4, "repeated calls must stay visible")
}

func TestOrganizeHandlesAnEmptyConversation(t *testing.T) {
	assert.Empty(t, conversation.Organize(nil))

	assert.Empty(t, conversation.Organize([]conversation.Message{}))
}

// Organize is used on the way to the provider. The engine's own history is the
// record of what happened and must not be rewritten under it.
func TestOrganizeDoesNotMutateItsInput(t *testing.T) {
	messages := []conversation.Message{
		testutils.Request("call_1", "shell", "{}"),
		{Type: conversation.TypeBot, Text: "thinking"},
		testutils.Response("call_1", "shell", "{}", "ok"),
	}

	before := kinds(messages)

	conversation.Organize(messages)

	assert.Equal(t, kinds(messages), before)
}

// The whole point, end to end. A history that trimming and interleaving have
// mangled still renders into something a provider accepts.
func TestOrganizeRepairsAHistoryOnTheWire(t *testing.T) {
	chat := conversation.ToPrompt([]conversation.Message{
		{Type: conversation.TypeInstructions, Text: litYouAreACoding},
		// this result's call fell outside the trimmed window
		testutils.Response("gone", "read", "{}", "old contents"),
		{Type: conversation.TypeUser, Text: "run the tests"},
		testutils.Request("call_1", "shell", `{"command":"go test"}`),
		{Type: conversation.TypeReasoning, Text: "waiting on it"},
		testutils.Response("call_1", "shell", `{"command":"go test"}`, "ok"),
		// and this call never got an answer before the run was interrupted
		testutils.Request("call_2", "shell", `{"command":"go vet"}`),
	})

	roles := make([]string, 0, len(chat))

	for _, message := range chat {
		roles = append(roles, string(message.Role))
	}

	want := []string{"system", "user", "assistant", "tool"}

	assert.Equal(t, want, roles)

	call, isCall := testutils.ToolCallOf(chat[2])
	result, isResult := chat[3].Content[0].(fantasy.ToolResultPart)

	assert.True(t, isCall, "the surviving pair must reference the same call id: %+v", chat[2:])
	assert.True(t, isResult, "the surviving pair must reference the same call id: %+v", chat[2:])
	assert.Equal(t, call.ToolCallID, result.ToolCallID, "the surviving pair must reference the same call id: %+v", chat[2:])
}
