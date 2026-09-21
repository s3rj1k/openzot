package loop_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/conversation"
	"github.com/openzot/openzot/internal/loop"
	"github.com/openzot/openzot/internal/provider"
)

// toolCalls is one model turn asking for several tools at once, finishing with
// the given reason.
func toolCalls(finish string, calls ...[3]string) string {
	parts := make([]string, len(calls))

	for i, call := range calls {
		parts[i] = fmt.Sprintf(
			`{"index":%d,"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}`,
			i, call[0], call[1], call[2])
	}

	return `{"choices":[{"delta":{"tool_calls":[` + strings.Join(parts, ",") +
		`]},"finish_reason":` + fmt.Sprintf("%q", finish) + `}]}`
}

func countTool(calls *int) fantasy.AgentTool {
	return namedTool(litEcho, func(context.Context) (any, error) {
		*calls++

		return "ok", nil
	})
}

// A terminal call ends the run before anything else of its turn is acted on. A
// shell command in the same turn as "success" is not run, and leaves nothing in
// the conversation.
func TestATerminalCallEndsTheRunBeforeItsSiblingsRun(t *testing.T) {
	ran := 0

	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client: stub(t, []string{toolCalls("tool_calls",
			[3]string{"c1", litEcho, `{}`},
			[3]string{"c2", loop.SuccessTool, `{"summary":"all done"}`},
		)}),
		Tools:      []fantasy.AgentTool{countTool(&ran)},
		Messages:   []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxSettles: 5,
	})

	assert.Equal(t, loop.StopSettled, result.Reason)
	assert.Equal(t, "all done", result.Message)

	assert.Equal(t, 0, ran, "want it not to run at all")

	requests, responses := countActivities(result.Messages)
	assert.Equal(t, 0, requests, "the conversation holds %d requests and %d responses, want none", requests, responses)
	assert.Equal(t, 0, responses, "the conversation holds %d requests and %d responses, want none", requests, responses)
}

// The call that would overrun the budget is not made, and does not leave a
// request with no answer behind. The next request would be invalid.
func TestTheCallBudgetStopsBeforeTheCallThatOverrunsIt(t *testing.T) {
	ran := 0

	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client: stub(t, []string{toolCalls("tool_calls",
			[3]string{"c1", litEcho, `{}`},
			[3]string{"c2", litEcho, `{}`},
		)}),
		Tools:    []fantasy.AgentTool{countTool(&ran)},
		Messages: []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxCalls: 1,
	})

	require.Equal(t, loop.StopCalls, result.Reason, "want the call budget to stop the run")

	assert.Equal(t, 1, ran, "want just the one call within the budget")

	requests, responses := countActivities(result.Messages)
	assert.Equal(t, 1, requests, "want one answered pair")
	assert.Equal(t, 1, responses, "want one answered pair")
}

// Endpoints in the wild end a turn that carries tool calls with "stop". The calls
// are what the model asked for and they are run.
func TestToolCallsAreRunWhateverTheProviderCalledTheEnding(t *testing.T) {
	for _, finish := range []string{"stop", "something_new"} {
		ran := 0

		result := run(t, &loop.Options{
			ContextWindow: testWindow,
			Client: stub(t,
				[]string{toolCalls(finish, [3]string{"c1", litEcho, `{}`})},
				[]string{settle("done")},
			),
			Tools:         []fantasy.AgentTool{countTool(&ran)},
			Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
			MaxIterations: 5,
		})

		assert.Equal(t, 1, ran, "finish %q: the tool ran %d times, want 1", finish, ran)

		assert.Equal(t, loop.StopSettled, result.Reason, "want the run to carry on and stop normally")
	}
}

// A turn cut off at the output limit can carry a call whose arguments were cut
// off with it. It is never run. The model is asked to continue.
func TestACallFromATruncatedTurnIsNeverRun(t *testing.T) {
	ran := 0

	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client: stub(t,
			[]string{toolCalls("length", [3]string{"c1", litEcho, `{}`})},
			[]string{settle("done")},
		),
		Tools:         []fantasy.AgentTool{countTool(&ran)},
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxIterations: 5,
	})

	assert.Equal(t, 0, ran, "want the cut-off call not to run")

	assert.Positive(t, result.Budget.Continuations+result.Budget.Recoveries, "a truncated turn must be continued, not accepted")
}

// The engine never leaves a conversation ending on the model's own words, but
// one it is handed might. Fantasy will not start from it, so it is given a line
// to continue from rather than failing the run.
func TestAConversationEndingOnTheModelsWordsStillRuns(t *testing.T) {
	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client:        stub(t, []string{settle("carrying on")}),
		Messages: []conversation.Message{
			{Type: conversation.TypeUser, Text: "go"},
			{Type: conversation.TypeBot, Text: "I began"},
		},
	})

	require.Equal(t, loop.StopSettled, result.Reason, "want the run to go ahead")
}

// A call to a tool that does not exist, or with input that cannot be read, is
// answered by fantasy without the tool being touched. It is still a call. It
// counts, and it is written into the conversation as a request and a failure.
func TestACallThatNeverReachedATool(t *testing.T) {
	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client: stub(t,
			[]string{toolCalls("tool_calls", [3]string{"c1", "missing", `{}`})},
			[]string{settle("noted")},
		),
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxIterations: 5,
	})

	assert.Equal(t, 1, result.Budget.Calls, "want the refused call counted")

	requests, responses := countActivities(result.Messages)
	assert.Equal(t, 1, requests, "want one pair")
	assert.Equal(t, 1, responses, "want one pair")

	assert.True(t, mentionsAFailure(result.Messages), "the refusal must be written down as a failure")
}

// A tool the engine was told never to repair is given no input the model did not
// finish writing. A command cut off mid-string goes back to the model, and does
// not run. The same slip in an ordinary tool is mended and the tool runs.
func TestAToolThatIsNeverRepairedRefusesAnUnfinishedCall(t *testing.T) {
	unfinished := `{"value": "rm -rf build`

	for _, test := range []struct {
		name       string
		unrepaired []string
		wantRan    int
	}{
		{"a listed tool is refused", []string{litEcho}, 0},
		{"an unlisted tool is repaired and run", nil, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			ran := 0

			result := run(t, &loop.Options{
				ContextWindow: testWindow,
				Client: stub(t,
					[]string{toolCalls("tool_calls", [3]string{"c1", litEcho, unfinished})},
					[]string{settle("done")},
				),
				Tools:         []fantasy.AgentTool{countTool(&ran)},
				Unrepaired:    test.unrepaired,
				Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
				MaxIterations: 5,
			})

			assert.Equal(t, test.wantRan, ran)

			assert.Equal(t, (test.wantRan == 0), mentionsAFailure(result.Messages))
		})
	}
}

// bodyOfTheFirstRequest runs one turn against a server that keeps what it was
// sent, with the given model settings.
func bodyOfTheFirstRequest(t *testing.T, tweak func(*provider.ClientConfig)) map[string]any {
	t.Helper()

	var body map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body == nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}

		w.Header().Set("Content-Type", "text/event-stream")

		fmt.Fprint(w, "data: "+text("hi")+"\n\ndata: "+stop()+"\n\ndata: [DONE]\n\n")
	}))

	t.Cleanup(server.Close)

	config := provider.ClientConfig{Provider: litCustom, Model: litTestModel, APIKey: "k", BaseURL: server.URL}
	tweak(&config)

	client, err := provider.NewClient(t.Context(), config)
	require.NoError(t, err)

	run(t, &loop.Options{ContextWindow: testWindow, Client: client, Messages: []conversation.Message{{Type: conversation.TypeUser, Text: "go"}}})

	require.NotNil(t, body, "the server saw no request")

	return body
}

// A model's reasoning_effort and extra_body go out with every request. A model
// with neither sends a request without them.
func TestAModelsRequestSettingsReachTheWire(t *testing.T) {
	body := bodyOfTheFirstRequest(t, func(c *provider.ClientConfig) {
		c.ReasoningEffort = "low"
		c.ExtraBody = map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}}
	})

	assert.Equal(t, "low", body["reasoning_effort"], "reasoning_effort = %v, want low", body["reasoning_effort"])

	kwargs, _ := body["chat_template_kwargs"].(map[string]any)
	thinking, ok := kwargs["enable_thinking"].(bool)
	assert.True(t, ok, "want the extra body merged in")
	assert.False(t, thinking, "want the extra body merged in")

	plain := bodyOfTheFirstRequest(t, func(*provider.ClientConfig) {})

	for _, key := range []string{"reasoning_effort", "chat_template_kwargs"} {
		_, sent := plain[key]
		assert.False(t, sent, "%s was sent by a model that asked for nothing", key)
	}
}

func kindsOf(kinds []loop.EventKind) []string {
	names := make([]string, len(kinds))

	for i, kind := range kinds {
		names[i] = string(kind)
	}

	return names
}

// OnEvent sees every event of a run, alongside whoever is watching it.
func TestOnEventSeesTheWholeRunAlongsideTheWatcher(t *testing.T) {
	var sunk, watched []loop.EventKind

	engine, err := loop.New(&loop.Options{
		ContextWindow: testWindow,
		Client:        stub(t, []string{settle("hi")}),
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		OnEvent:       func(event loop.Event) { sunk = append(sunk, event.Kind) },
	})
	require.NoError(t, err)

	engine.Run(t.Context(), func(event loop.Event) { watched = append(watched, event.Kind) })

	assert.NotEmpty(t, sunk, "want the same events")
	assert.Equal(t, strings.Join(kindsOf(watched), ","), strings.Join(kindsOf(sunk), ","), "want the same events")
}

// A model calling a tool with no parameters often sends "" for the arguments.
// The call is announced with an empty object, not with nothing.
func TestAnEmptyInputIsAnEmptyObject(t *testing.T) {
	for _, input := range []string{"", "  ", "{}"} {
		arguments := loop.DecodeInput(input)
		assert.NotNil(t, arguments, "want an empty object")
		assert.Empty(t, arguments, "want an empty object")
	}

	arguments := loop.DecodeInput("[1]")
	assert.Nil(t, arguments, "input that is not an object decoded to %v, want nil", arguments)
}
