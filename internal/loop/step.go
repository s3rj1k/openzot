package loop

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/jsonrepair"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/openzot/openzot/internal/conversation"
)

// errRunaway is what the streaming guard ends a degenerate turn with. It is
// returned from a stream callback to stop fantasy reading the stream, then
// recognised by runStep, which keeps what the turn had said so far.
var errRunaway = errors.New("loop: the turn ran away")

// turnResult is what one model call produced.
type turnResult struct {
	Text         string
	Reasoning    string
	ToolCalls    []fantasy.ToolCallContent
	FinishReason fantasy.FinishReason

	// InputTokens and OutputTokens are the prompt- and completion-token counts the
	// provider reported for this turn (zero when it reported none). Provider counts,
	// not the local estimate: they reflect what the provider actually processed,
	// including any server-side prompt caching.
	InputTokens  int
	OutputTokens int
}

// turnRequest is what one model call is sent: the conversation to show, without the
// system prompt, which the agent carries.
type turnRequest struct {
	messages  []fantasy.Message
	maxOutput *int64
}

// step is one iteration's model call, and the tools it runs, as the engine sees
// them.
//
// fantasy's Agent does the work of a step - the call, checking and repairing the
// tool calls, running the tools - and this is what keeps the engine's own
// conversation in step with it: it turns the agent's callbacks into events and
// into the messages the conversation is made of, in the order the engine has
// always written them. One step is used for the whole run and reset per
// iteration.
type step struct {
	engine   *Engine
	messages *[]conversation.Message
	budget   *Budget
	emit     func(Event)

	turn      turnResult
	text      strings.Builder
	reasoning strings.Builder
	guard     *runawayGuard
	runaway   *guardReason

	// flushed is whether the turn's reasoning and words are in the conversation
	// yet. They go in before the first tool call of the turn does, and are
	// otherwise written once the model call is over.
	flushed bool

	// terminalSeen is set when the turn calls a terminal tool. The run ends
	// there, before any other call of the turn is acted on.
	terminalSeen bool

	// callsExhausted is set when the call budget runs out mid-turn; the calls
	// from there on are not run.
	callsExhausted bool

	// started holds the calls the engine's own wrapper began, so a result for
	// one is not recorded twice.
	started map[string]bool
}

func (s *step) reset(messages *[]conversation.Message, budget *Budget, emit func(Event)) {
	minChars := RunawayGuardMinChars

	*s = step{
		engine:   s.engine,
		messages: messages,
		budget:   budget,
		emit:     emit,
		guard:    newRunawayGuard(guardOptions{MinChars: &minChars}),
		started:  map[string]bool{},
	}
}

// newAgent builds the agent a run uses: the instructions, and every tool - the
// terminal ones too - wrapped so the engine sees each call.
func (e *Engine) newAgent(state *step) fantasy.Agent {
	offered := append([]fantasy.AgentTool(nil), e.options.Tools...)
	offered = append(offered, terminalTools()...)

	wrapped := make([]fantasy.AgentTool, len(offered))

	for i, tool := range offered {
		wrapped[i] = guardedTool{AgentTool: tool, step: state}
	}

	options := []fantasy.AgentOption{
		fantasy.WithSystemPrompt(e.instructions()),
		fantasy.WithTools(wrapped...),

		// No retries here: the engine retries, on its own schedule and budget.
		fantasy.WithMaxRetries(0),
	}

	if provider := e.providerOptions(); provider != nil {
		options = append(options, fantasy.WithProviderOptions(provider))
	}

	return fantasy.NewAgent(e.options.Client.Model(), options...)
}

// providerOptions is what the model's config asks fantasy to send beyond the
// conversation itself, or nil when it asks for nothing.
func (e *Engine) providerOptions() fantasy.ProviderOptions {
	config := e.options.Client.Config()

	if config.ReasoningEffort == "" && len(config.ExtraBody) == 0 {
		return nil
	}

	options := &openaicompat.ProviderOptions{ExtraBody: config.ExtraBody}

	if config.ReasoningEffort != "" {
		effort := openai.ReasoningEffort(config.ReasoningEffort)

		options.ReasoningEffort = &effort
	}

	return openaicompat.NewProviderOptions(options)
}

// repairToolCall is fantasy's own repair - mend the JSON - except for the tools
// the engine was told never to repair, whose calls are refused as they stand.
func (e *Engine) repairToolCall(_ context.Context, options fantasy.ToolCallRepairOptions) (*fantasy.ToolCallContent, error) {
	call := options.OriginalToolCall

	if slices.Contains(e.options.Unrepaired, call.ToolName) {
		return nil, options.ValidationError
	}

	repaired, err := jsonrepair.RepairJSON(call.Input)
	if err != nil || repaired == call.Input {
		return nil, options.ValidationError
	}

	call.Input = repaired

	return &call, nil
}

// runStep performs one model call and runs the tools it asks for, streaming its
// output through emit and watching for a runaway.
func (e *Engine) runStep(
	ctx context.Context,
	agent fantasy.Agent,
	state *step,
	call turnRequest,
	messages *[]conversation.Message, budget *Budget,
	emit func(Event),
) (turnResult, error) {
	// A turn can end while the provider is still streaming - the runaway guard
	// cuts a degenerate one short - so every exit from here cancels the stream,
	// which closes the response body rather than leaving it open for the life
	// of the process.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	state.reset(messages, budget, emit)

	_, err := agent.Stream(ctx, fantasy.AgentStreamCall{
		Messages:        call.messages,
		MaxOutputTokens: call.maxOutput,

		// one step and no more: whether to go round again is the engine's call
		StopWhen: []fantasy.StopCondition{func([]fantasy.StepResult) bool { return true }},

		// set on the call, not on the agent: Agent.Stream reads the repair function
		// from the call alone
		RepairToolCall: e.repairToolCall,

		OnTextDelta:      state.onTextDelta,
		OnReasoningDelta: state.onReasoningDelta,
		OnStreamFinish:   state.onStreamFinish,
		OnToolCall:       state.onToolCall,
		OnToolResult:     state.onToolResult,
	})

	if errors.Is(err, errRunaway) {
		err = nil

		state.turn.FinishReason = fantasy.FinishReasonStop

		emit(Event{Kind: EventRunaway, Text: state.runaway.Text})
	}

	if err != nil {
		return turnResult{}, err
	}

	state.turn.Text = state.text.String()
	state.turn.Reasoning = state.reasoning.String()

	return state.turn, nil
}

func (s *step) onTextDelta(_, delta string) error {
	s.text.WriteString(delta)

	s.emit(Event{Kind: EventToken, Text: delta})

	// the streaming guard cuts a degenerate turn short rather than letting it
	// burn the whole output budget
	if s.guard.Push(delta) {
		s.runaway = s.guard.Reason()

		if s.runaway != nil {
			return errRunaway
		}
	}

	return nil
}

func (s *step) onReasoningDelta(_, delta string) error {
	s.reasoning.WriteString(delta)

	s.emit(Event{Kind: EventReasoningToken, Text: delta})

	return nil
}

func (s *step) onStreamFinish(usage fantasy.Usage, reason fantasy.FinishReason, _ fantasy.ProviderMetadata) error {
	s.turn.FinishReason = reason

	// the provider's own count, which reflects what it actually processed
	// (server-side prompt caching and all) - never the local estimate
	// fantasy reports the prompt without its cached part; zot has always
	// counted the whole prompt, so the cached tokens are added back
	if prompt := usage.InputTokens + usage.CacheReadTokens; prompt > 0 {
		s.turn.InputTokens = int(prompt)
	}

	s.turn.OutputTokens = int(usage.OutputTokens)

	return nil
}

// onToolCall is told of every call of a turn before any of them runs.
func (s *step) onToolCall(call fantasy.ToolCallContent) error {
	// what the turn said comes before what it did
	s.flush()

	s.turn.ToolCalls = append(s.turn.ToolCalls, call)

	if call.ToolName == SuccessTool || call.ToolName == FailureTool {
		s.terminalSeen = true
	}

	return nil
}

// flush puts the turn's reasoning and words into the conversation, once.
func (s *step) flush() {
	if s.flushed {
		return
	}

	s.flushed = true

	if reasoning := s.reasoning.String(); reasoning != "" {
		*s.messages = append(*s.messages, conversation.Message{Type: conversation.TypeReasoning, Text: reasoning})

		s.emit(Event{Kind: EventMessage, MessageType: conversation.TypeReasoning, Text: reasoning})
	}

	if text := s.text.String(); text != "" {
		*s.messages = append(*s.messages, conversation.Message{Type: conversation.TypeBot, Text: text})

		s.emit(Event{Kind: EventMessage, MessageType: conversation.TypeBot, Text: text})
	}
}

// onToolResult records the calls that never reached a tool: one to a tool that
// does not exist, or whose input could not be read even after repair. fantasy
// answers those itself, so the wrapper never sees them.
func (s *step) onToolResult(result fantasy.ToolResultContent) error {
	if s.started[result.ToolCallID] || s.terminalSeen || s.callsExhausted {
		return nil
	}

	if !s.spendCall() {
		return nil
	}

	call := s.callOf(result.ToolCallID)

	failure := "the call could not be run"

	if refusal, ok := result.Result.(fantasy.ToolResultOutputContentError); ok && refusal.Error != nil {
		failure = refusal.Error.Error()
	}

	s.begin(call, false)
	s.end(call, "", failure)

	return nil
}

// spendCall counts a tool call against the budget, or reports that there is none
// left. The call that would go over it is not made and leaves nothing behind.
func (s *step) spendCall() bool {
	if s.engine.maxCalls > 0 && s.budget.Calls >= s.engine.maxCalls {
		s.callsExhausted = true

		return false
	}

	s.budget.Calls++

	return true
}

func (s *step) callOf(id string) fantasy.ToolCallContent {
	for _, call := range s.turn.ToolCalls {
		if call.ToolCallID == id {
			return call
		}
	}

	return fantasy.ToolCallContent{ToolCallID: id}
}

// begin writes a call's request into the conversation and announces it. With
// handOver set the conversation is given to the caller too, before the tool runs:
// a shell call can outlast the run, and a run killed inside one must still leave
// what the model thought and asked for.
func (s *step) begin(call fantasy.ToolCallContent, handOver bool) {
	*s.messages = append(*s.messages, activityMessage(conversation.ActivityRequest, call, nil, ""))

	s.emit(Event{Kind: EventToolCallStart, Tool: call.ToolName, Args: decodeInput(call.Input), Text: call.Input})

	if handOver {
		s.engine.handOver(*s.messages)
	}
}

// end writes a call's answer into the conversation and announces it: a failure
// when there is one, otherwise the tool's output.
func (s *step) end(call fantasy.ToolCallContent, output, failure string) {
	if failure != "" {
		s.emit(Event{Kind: EventToolCallError, Tool: call.ToolName, Text: failure})

		*s.messages = append(*s.messages, activityMessage(conversation.ActivityResponse, call, nil, failure))

		return
	}

	s.emit(Event{Kind: EventToolCallEnd, Tool: call.ToolName, Result: output})

	*s.messages = append(*s.messages, activityMessage(conversation.ActivityResponse, call, output, ""))
}

// guardedTool is a tool as fantasy runs it, with the engine looking on: it is
// where the conversation and the events learn of a call being made, in the same
// order and at the same moments as ever - the request is written, and handed over,
// before the tool runs, and the answer after.
type guardedTool struct {
	fantasy.AgentTool
	step *step
}

func (g guardedTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	s := g.step

	// A terminal call ends the run before anything else of its turn is acted on,
	// and a spent call budget ends it before the call that would overrun it.
	// Neither leaves a trace in the conversation.
	if s.terminalSeen || s.callsExhausted || !s.spendCall() {
		return fantasy.NewTextResponse("skipped"), nil
	}

	s.started[call.ID] = true

	content := fantasy.ToolCallContent{ToolCallID: call.ID, ToolName: call.Name, Input: call.Input}

	s.begin(content, true)

	response, err := g.AgentTool.Run(ctx, call)

	// a tool that ran and reported a problem is answered the same way as one that
	// could not run: the model reads the failure and acts on it
	if err == nil && response.IsError {
		err = errors.New(response.Content)
	}

	if err != nil {
		s.end(content, "", err.Error())

		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	s.end(content, response.Content, "")

	return response, nil
}

// decodeInput reads a call's JSON input for the event that announces it. An empty
// input is an empty object, and input that is not an object is nil: the raw text
// travels with the event either way.
func decodeInput(input string) map[string]any {
	input = strings.TrimSpace(input)

	if input == "" {
		return map[string]any{}
	}

	var arguments map[string]any

	if err := json.Unmarshal([]byte(input), &arguments); err != nil {
		return nil
	}

	return arguments
}
