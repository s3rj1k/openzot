package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/openzot/openzot/internal/conversation"
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

// A terminal call ends the run before anything else of its turn is acted on: a
// shell command in the same turn as "success" is not run, and leaves nothing in
// the conversation.
func TestATerminalCallEndsTheRunBeforeItsSiblingsRun(t *testing.T) {
	ran := 0

	result := run(t, Options{
		ContextWindow: testWindow,
		Client: stub(t, []string{toolCalls("tool_calls",
			[3]string{"c1", litEcho, `{}`},
			[3]string{"c2", SuccessTool, `{"summary":"all done"}`},
		)}),
		Tools:      []fantasy.AgentTool{countTool(&ran)},
		Messages:   []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxSettles: 5,
	})

	if result.Reason != StopSettled || result.Message != "all done" {
		t.Errorf("reason = %q, message = %q, want settled with the summary", result.Reason, result.Message)
	}

	if ran != 0 {
		t.Errorf("the sibling tool ran %d times, want it not to run at all", ran)
	}

	if requests, responses := countActivities(result.Messages); requests != 0 || responses != 0 {
		t.Errorf("the conversation holds %d requests and %d responses, want none", requests, responses)
	}
}

// The call that would overrun the budget is not made, and does not leave a
// request with no answer behind: the next request would be invalid.
func TestTheCallBudgetStopsBeforeTheCallThatOverrunsIt(t *testing.T) {
	ran := 0

	result := run(t, Options{
		ContextWindow: testWindow,
		Client: stub(t, []string{toolCalls("tool_calls",
			[3]string{"c1", litEcho, `{}`},
			[3]string{"c2", litEcho, `{}`},
		)}),
		Tools:    []fantasy.AgentTool{countTool(&ran)},
		Messages: []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxCalls: 1,
	})

	if result.Reason != StopCalls {
		t.Fatalf("reason = %q, want the call budget to stop the run", result.Reason)
	}

	if ran != 1 {
		t.Errorf("the tool ran %d times, want just the one call within the budget", ran)
	}

	if requests, responses := countActivities(result.Messages); requests != 1 || responses != 1 {
		t.Errorf("the conversation holds %d requests and %d responses, want one answered pair", requests, responses)
	}
}

// Endpoints in the wild end a turn that carries tool calls with "stop". The calls
// are what the model asked for and they are run.
func TestToolCallsAreRunWhateverTheProviderCalledTheEnding(t *testing.T) {
	for _, finish := range []string{"stop", "something_new"} {
		ran := 0

		result := run(t, Options{
			ContextWindow: testWindow,
			Client: stub(t,
				[]string{toolCalls(finish, [3]string{"c1", litEcho, `{}`})},
				[]string{settle("done")},
			),
			Tools:         []fantasy.AgentTool{countTool(&ran)},
			Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
			MaxIterations: 5,
		})

		if ran != 1 {
			t.Errorf("finish %q: the tool ran %d times, want 1", finish, ran)
		}

		if result.Reason != StopSettled {
			t.Errorf("finish %q: reason = %q, want the run to carry on and stop normally", finish, result.Reason)
		}
	}
}

// A turn cut off at the output limit can carry a call whose arguments were cut
// off with it. It is never run; the model is asked to continue.
func TestACallFromATruncatedTurnIsNeverRun(t *testing.T) {
	ran := 0

	result := run(t, Options{
		ContextWindow: testWindow,
		Client: stub(t,
			[]string{toolCalls("length", [3]string{"c1", litEcho, `{}`})},
			[]string{settle("done")},
		),
		Tools:         []fantasy.AgentTool{countTool(&ran)},
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxIterations: 5,
	})

	if ran != 0 {
		t.Errorf("the tool ran %d times, want the cut-off call not to run", ran)
	}

	if result.Budget.Continuations == 0 && result.Budget.Recoveries == 0 {
		t.Error("a truncated turn must be continued, not accepted")
	}
}

// The engine never leaves a conversation ending on the model's own words, but
// one it is handed might; fantasy will not start from it, so it is given a line
// to continue from rather than failing the run.
func TestAConversationEndingOnTheModelsWordsStillRuns(t *testing.T) {
	result := run(t, Options{
		ContextWindow: testWindow,
		Client:        stub(t, []string{settle("carrying on")}),
		Messages: []conversation.Message{
			{Type: conversation.TypeUser, Text: "go"},
			{Type: conversation.TypeBot, Text: "I began"},
		},
	})

	if result.Reason != StopSettled {
		t.Fatalf("reason = %q, err = %v, want the run to go ahead", result.Reason, result.Err)
	}
}

// A call to a tool that does not exist, or with input that cannot be read, is
// answered by fantasy without the tool being touched. It is still a call: it
// counts, and it is written into the conversation as a request and a failure.
func TestACallThatNeverReachedATool(t *testing.T) {
	result := run(t, Options{
		ContextWindow: testWindow,
		Client: stub(t,
			[]string{toolCalls("tool_calls", [3]string{"c1", "missing", `{}`})},
			[]string{settle("noted")},
		),
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxIterations: 5,
	})

	if result.Budget.Calls != 1 {
		t.Errorf("calls = %d, want the refused call counted", result.Budget.Calls)
	}

	if requests, responses := countActivities(result.Messages); requests != 1 || responses != 1 {
		t.Errorf("the conversation holds %d requests and %d responses, want one pair", requests, responses)
	}

	if !mentionsAFailure(result.Messages) {
		t.Error("the refusal must be written down as a failure")
	}
}

// A tool the engine was told never to repair is given no input the model did not
// finish writing: a command cut off mid-string goes back to the model, and does
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

			result := run(t, Options{
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

			if ran != test.wantRan {
				t.Errorf("the tool ran %d times, want %d", ran, test.wantRan)
			}

			if refused := mentionsAFailure(result.Messages); refused != (test.wantRan == 0) {
				t.Errorf("a failure was recorded = %v, want %v", refused, test.wantRan == 0)
			}
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
	if err != nil {
		t.Fatal(err)
	}

	run(t, Options{ContextWindow: testWindow, Client: client, Messages: []conversation.Message{{Type: conversation.TypeUser, Text: "go"}}})

	if body == nil {
		t.Fatal("the server saw no request")
	}

	return body
}

// A model's reasoning_effort and extra_body go out with every request; a model
// with neither sends a request without them.
func TestAModelsRequestSettingsReachTheWire(t *testing.T) {
	body := bodyOfTheFirstRequest(t, func(c *provider.ClientConfig) {
		c.ReasoningEffort = "low"
		c.ExtraBody = map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}}
	})

	if body["reasoning_effort"] != "low" {
		t.Errorf("reasoning_effort = %v, want low", body["reasoning_effort"])
	}

	kwargs, _ := body["chat_template_kwargs"].(map[string]any)
	if thinking, ok := kwargs["enable_thinking"].(bool); !ok || thinking {
		t.Errorf("chat_template_kwargs = %v, want the extra body merged in", body["chat_template_kwargs"])
	}

	plain := bodyOfTheFirstRequest(t, func(*provider.ClientConfig) {})

	for _, key := range []string{"reasoning_effort", "chat_template_kwargs"} {
		if _, sent := plain[key]; sent {
			t.Errorf("%s was sent by a model that asked for nothing", key)
		}
	}
}

// OnEvent sees every event of a run, alongside whoever is watching it.
func TestOnEventSeesTheWholeRunAlongsideTheWatcher(t *testing.T) {
	var sunk, watched []EventKind

	engine, err := New(Options{
		ContextWindow: testWindow,
		Client:        stub(t, []string{settle("hi")}),
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		OnEvent:       func(event Event) { sunk = append(sunk, event.Kind) },
	})
	if err != nil {
		t.Fatal(err)
	}

	engine.Run(t.Context(), func(event Event) { watched = append(watched, event.Kind) })

	if len(sunk) == 0 || strings.Join(kindsOf(sunk), ",") != strings.Join(kindsOf(watched), ",") {
		t.Errorf("sink saw %v, watcher saw %v, want the same events", sunk, watched)
	}
}

func kindsOf(kinds []EventKind) []string {
	names := make([]string, len(kinds))

	for i, kind := range kinds {
		names[i] = string(kind)
	}

	return names
}

// A model calling a tool with no parameters often sends "" for the arguments;
// the call is announced with an empty object, not with nothing.
func TestAnEmptyInputIsAnEmptyObject(t *testing.T) {
	for _, input := range []string{"", "  ", "{}"} {
		if arguments := decodeInput(input); arguments == nil || len(arguments) != 0 {
			t.Errorf("decodeInput(%q) = %v, want an empty object", input, arguments)
		}
	}

	if arguments := decodeInput("[1]"); arguments != nil {
		t.Errorf("input that is not an object decoded to %v, want nil", arguments)
	}
}
