package loop

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/schema"

	"github.com/openzot/openzot/internal/conversation"
	"github.com/openzot/openzot/internal/provider"
)

// Options configures a run.
type Options struct {
	// Client is the provider connection.
	Client *provider.Client

	// Instructions is the system prompt.
	Instructions string

	// Messages seeds the conversation.
	Messages []conversation.Message

	// Tools the model may call.
	Tools []fantasy.AgentTool

	// Tools whose calls are never repaired. fantasy mends input that does not parse, which is the wrong
	// trade for a tool that acts on the machine. Such a call goes back to the model as it stands.
	Unrepaired []string

	// Called at each iteration boundary and again before each tool handler, with the whole conversation
	// as it stands, so a run killed at iteration 500 still leaves its record.

	// The engine only appends to it. Only the copy sent on the wire is trimmed to the window.
	OnConversation func([]conversation.Message)

	// Sees every event alongside the function Run is given, for a sink that must see the whole run
	// whoever is watching, such as a session log.
	OnEvent func(Event)

	// MaxIterations, MaxContinuations, MaxCycles, MaxEmpties bound the run. Zero
	// uses the corresponding default.
	MaxIterations    int
	MaxContinuations int
	MaxCycles        int
	MaxEmpties       int

	// Bounds recovery attempts across the whole run, however spaced. See DefaultMaxRecoveries for why
	// it is separate from MaxContinuations. Zero uses the default.
	MaxRecoveries int

	// MaxCalls bounds total tool calls. Zero (or negative) is unbounded - only
	// the iteration count is a hard default fallback.
	MaxCalls int

	// MaxDuration bounds the wall-clock time of a run, checked at each iteration
	// boundary. Zero is unbounded.
	MaxDuration time.Duration

	// Pause before the first retry of a retriable failure, doubling per consecutive retry up to
	// MaxRetryBackoff. Zero uses DefaultRetryBackoff. Negative disables the wait, for tests driving an outage.
	RetryBackoff time.Duration

	// How many times the model is nudged to record an outcome before the run is reported unsettled.
	// Zero uses the default. Settling cannot be turned off, since only a terminal tool call ends a run.
	MaxSettles int

	// MaxTokens bounds a single response.
	MaxTokens *int

	// The tool the model keeps its plan with. Empty turns plan handling off. The engine does not know
	// its schema, since the plan is simply the latest successful call.
	PlanTool string

	// PlanNudgeEvery is how many iterations pass between reminders of the plan
	// tool. Zero uses DefaultPlanNudgeEvery. Negative turns the reminders off.
	PlanNudgeEvery int

	// Turns the window must still hold after forgetting for the plan to stay where it is. Fewer and it
	// is posted again. Zero uses DefaultPlanMinTurns.
	PlanMinTurns int

	// Percent of the window where the oldest messages start to be forgotten (one per request) and
	// where as many as needed are, so the request stays under it. Zero uses the defaults.
	ContextSoft int
	ContextHard int

	// The model's total context window, in tokens. Required, and New rejects a run without it. There
	// is no built-in table, since only the operator knows the real ceiling of the endpoint.
	ContextWindow int
}

// Result is the outcome of a run.
type Result struct {
	// Reason is why the run ended.
	Reason StopReason

	// Message is a human-readable explanation.
	Message string

	// Messages is the conversation as it ended.
	Messages []conversation.Message

	// Budget is what the run spent.
	Budget Budget

	// Err is set when Reason is StopError.
	Err error
}

// ExitCode is the process-style exit code the run's ending maps onto. Zero means the model settled with success, and
// everything else means the task did not get done, whether the model declared it could not (StopFailed) or a guard cut it
// short. A caller scripting against zot can tell those apart from success without parsing prose.
func (r *Result) ExitCode() int {
	switch r.Reason {
	case StopSettled:
		return 0
	default:
		return 1
	}
}

// Engine runs conversations.
type Engine struct {
	options Options

	maxIterations    int
	maxCalls         int
	maxDuration      time.Duration
	maxContinuations int

	// maxRecoveries is the runaway fallback under the consecutive count. See
	// DefaultMaxRecoveries.
	maxRecoveries int
	maxCycles     int
	maxEmpties    int
	maxSettles    int
	retryBackoff  time.Duration

	// window is the context window requests are held under. The configured one,
	// lowered when a provider rejects a request and states its own ceiling.
	window      int
	planEvery   int
	planTurns   int
	softPercent int
	hardPercent int

	// toolTokens caches the cost of the tool schemas, which are the same on
	// every request of a run and would otherwise be re-counted each round.
	toolTokens int
}

// toolSchemaTokens is what the tool definitions cost on the wire. They go with every request and can run to thousands of
// tokens, so leaving them out of the budget is how a request that seems to fit gets rejected.
func (e *Engine) toolSchemaTokens(tools []fantasy.Tool) int {
	if e.toolTokens > 0 || len(tools) == 0 {
		return e.toolTokens
	}

	encoded, err := json.Marshal(tools)
	if err != nil {
		return 0
	}

	e.toolTokens = conversation.EstimateTokens(string(encoded))

	return e.toolTokens
}

// New creates an engine, applying defaults.
func New(options *Options) (*Engine, error) {
	if options.Client == nil {
		return nil, errors.New("loop: no provider client")
	}

	if options.ContextWindow <= 0 {
		return nil, errors.New("loop: no context window: set context on the model in the config")
	}

	pick := func(value, fallback int) int {
		if value > 0 {
			return value
		}

		return fallback
	}

	planEvery := options.PlanNudgeEvery

	if planEvery == 0 {
		planEvery = DefaultPlanNudgeEvery
	}

	return &Engine{
		options:       *options,
		maxIterations: pick(options.MaxIterations, DefaultMaxIterations),
		// Calls and time are unbounded unless the caller sets them, and only the iteration count is a hard
		// default. A non-positive value means no cap, so they are stored raw rather than picked.
		maxCalls:         max(options.MaxCalls, 0),
		maxDuration:      options.MaxDuration,
		maxContinuations: pick(options.MaxContinuations, DefaultMaxContinuations),
		maxRecoveries:    pick(options.MaxRecoveries, DefaultMaxRecoveries),
		maxCycles:        pick(options.MaxCycles, DefaultMaxCycles),
		maxEmpties:       pick(options.MaxEmpties, DefaultMaxEmpties),
		maxSettles:       pick(options.MaxSettles, DefaultMaxSettles),
		// @note negative means "no wait" and is stored raw, so a test driving an
		// outage does not have to sleep through it. Zero takes the default.
		retryBackoff: cmp.Or(options.RetryBackoff, DefaultRetryBackoff),
		window:       options.ContextWindow,
		softPercent:  pick(options.ContextSoft, DefaultContextSoft),
		planEvery:    planEvery,
		planTurns:    pick(options.PlanMinTurns, DefaultPlanMinTurns),
		hardPercent:  pick(options.ContextHard, DefaultContextHard),
	}, nil
}

// canContinue reports whether another recovery attempt is within both bounds, the consecutive run and the total across the
// run. Both are checked wherever one is spent, so neither can be dodged by a different route into recovery.
func (e *Engine) canContinue(budget Budget) bool {
	return budget.Continuations < e.maxContinuations &&
		budget.Recoveries < e.maxRecoveries
}

// backoffFor is the pause before the attempt'th consecutive retry. It is base, doubling per attempt, capped at
// MaxRetryBackoff even when base alone exceeds the cap, so no generous RetryBackoff makes the first retry the longest wait.
// A non-positive base means no wait at all.
func backoffFor(base time.Duration, attempt int) time.Duration {
	if base <= 0 || attempt <= 0 {
		return 0
	}

	delay := base

	if delay >= MaxRetryBackoff {
		return MaxRetryBackoff
	}

	for i := 1; i < attempt; i++ {
		delay *= 2

		if delay >= MaxRetryBackoff {
			return MaxRetryBackoff
		}
	}

	return delay
}

// rateLimitWait is how long to sit out a rate limit, the larger of the provider's advised delay and the ordinary backoff. The
// backoff is a floor, so a "Retry-After: 0" cannot become a tight loop. The advice is capped, since an unattended run must not
// be parked for hours by a mistaken or hostile header.
func rateLimitWait(advised time.Duration, ok bool, fallback time.Duration) time.Duration {
	if !ok {
		return fallback
	}

	if advised > MaxRateLimitWait {
		advised = MaxRateLimitWait
	}

	if advised < fallback {
		return fallback
	}

	return advised
}

// wait pauses for d, or until the run is canceled - whichever comes first. It
// does not report which. The caller loops back to the cancellation check at the
// top of Run, so a canceled wait ends the run there rather than in two places.
func wait(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// firstNonNil returns a if it is set, else b - so an abort prefers the
// provider failure worth diagnosing over the bare cancellation.
func firstNonNil(a, b error) error {
	if a != nil {
		return a
	}

	return b
}

// terminalDetail pulls the explanation out of a terminal call's arguments.
func terminalDetail(call fantasy.ToolCallContent, key, fallback string) string {
	if value, ok := decodeInput(call.Input)[key].(string); ok && value != "" {
		return value
	}

	return fallback
}

// terminalCall reports whether the model ended the run with a terminal tool.
func terminalCall(calls []fantasy.ToolCallContent) (StopReason, string, bool) {
	for _, call := range calls {
		switch call.ToolName {
		case SuccessTool:
			return StopSettled, terminalDetail(call, "summary", "task complete"), true
		case FailureTool:
			return StopFailed, terminalDetail(call, "reason", "task failed"), true
		}
	}

	return "", "", false
}

// handOver gives the conversation as it stands to the OnConversation hook.
func (e *Engine) handOver(messages []conversation.Message) {
	if e.options.OnConversation != nil {
		e.options.OnConversation(messages)
	}
}

// cycleDetail turns a heuristic name into something the model can act on.
func cycleDetail(heuristic string) string {
	switch heuristic {
	case "repeated_result_run":
		return "you have called the same tool with the same arguments and received the same result several times"
	case "repeated_activity_tail":
		return "your recent tool calls keep cycling through the same small set of actions"
	case "repeated_suffix":
		return "the last few turns of this conversation are an exact repeat of the ones before them"
	case "repeated_message_text_run":
		return "your last answer repeated the same sentences over and over"
	default:
		return ""
	}
}

func finish(messages []conversation.Message, budget Budget, reason StopReason, detail string, err error) Result {
	return Result{
		Reason:   reason,
		Message:  detail,
		Messages: messages,
		Budget:   budget,
		Err:      err,
	}
}

// checkCycle looks for repetition and nudges the model, or stops the run once
// nudging has failed enough times.
func (e *Engine) checkCycle(messages []conversation.Message, budget *Budget) ([]conversation.Message, *Result) {
	detected := describeCycle(messages)

	if detected == "" {
		// A round that is not cyclic breaks the run of repetitions. The budget counts consecutive cycles, so
		// two unrelated repetitions far apart must not add up to a stop.
		budget.Cycles = 0

		return nil, nil
	}

	if budget.Cycles >= e.maxCycles {
		result := finish(messages, *budget, StopCycle,
			fmt.Sprintf("the model kept repeating itself (%s)", detected), nil)

		return nil, &result
	}

	budget.Cycles++

	return append(messages, conversation.Message{Type: conversation.TypeUser, Text: cycleNotice(cycleDetail(detected))}), nil
}

// narrowWindow lowers the context window requests are held under after a provider rejected one as too long, and reports whether
// it went down. The provider's stated window beats the configured one, since a rejection means the configured one was wrong.
// Without a usable number the window steps down a quarter, to a floor. Only the window changes, not the conversation.
func (e *Engine) narrowWindow(limit provider.ContextLimit, emit func(Event)) bool {
	if limit.SuggestedLimit > 0 && limit.SuggestedLimit < e.window {
		e.window = limit.SuggestedLimit

		emit(Event{Kind: EventRetry, Text: fmt.Sprintf(
			"provider reported a %d token window; retrying under %d",
			limit.MaxTokens, limit.SuggestedLimit)})

		return true
	}

	narrowed := e.window * 3 / 4

	if narrowed < e.options.ContextWindow/narrowFloor {
		return false
	}

	e.window = narrowed

	emit(Event{Kind: EventRetry, Text: fmt.Sprintf(
		"provider rejected the request as too long; retrying under %d tokens", narrowed)})

	return true
}

// turnsHeld is how many whole turns the window still holds. The turn about to be
// asked for is the last of turnStarts and has not happened yet, so it is not one.
func turnsHeld(turnStarts []int, forgotten int) int {
	turns := 0

	for _, start := range turnStarts[:len(turnStarts)-1] {
		if start >= forgotten {
			turns++
		}
	}

	return turns
}

// instructions renders the system prompt.
func (e *Engine) instructions() string {
	var builder strings.Builder

	builder.WriteString(e.options.Instructions)

	fmt.Fprintf(&builder,
		"\n\nWhen the objective is met, call %s. If it cannot be met, call %s. "+
			"The run is not finished until you call one of them.",
		SuccessTool, FailureTool,
	)

	return builder.String()
}

// forgetOldest moves the offset forward as far as the window calls for, and
// reports whether it moved.
func (e *Engine) forgetOldest(messages []conversation.Message, forgotten *int, tools []fantasy.Tool, emit func(Event)) bool {
	// the system prompt and the tool schemas are sent on every request and are
	// part of what fills the window
	used := conversation.EstimateTokens(e.instructions()) + e.toolSchemaTokens(tools)

	for _, message := range messages[*forgotten:] {
		used += conversation.Cost(message)
	}

	next := conversation.Forget(messages, *forgotten, used, e.window, e.softPercent, e.hardPercent, conversation.Cost)
	if next == *forgotten {
		return false
	}

	emit(Event{Kind: EventNotice, Text: fmt.Sprintf(
		"forgot %d older messages to stay within the context window", next-*forgotten)})

	*forgotten = next

	return true
}

// repostedPlan is the model's latest plan, its last successful plan-tool call, as a fresh call and result to append. It reports
// false when there is no plan or the plan is still in the window and needs no help.
func (e *Engine) repostedPlan(messages []conversation.Message, forgotten int) ([]conversation.Message, bool) {
	if e.options.PlanTool == "" {
		return nil, false
	}

	for index, message := range slices.Backward(messages) {
		activity := message.Activity

		if activity == nil || activity.Kind != conversation.ActivityResponse || activity.Name != e.options.PlanTool || activity.Failure != "" {
			continue
		}

		if index >= forgotten {
			return nil, false
		}

		id := fmt.Sprintf("plan-%d", len(messages))

		call := conversation.Activity{Kind: conversation.ActivityRequest, ID: id, Name: activity.Name, Arguments: activity.Arguments}
		answer := conversation.Activity{Kind: conversation.ActivityResponse, ID: id, Name: activity.Name, Arguments: activity.Arguments, Result: activity.Result}

		return []conversation.Message{
			{Type: conversation.TypeActivity, Activity: &call},
			{Type: conversation.TypeActivity, Text: answer.ResultText(), Activity: &answer},
		}, true
	}

	return nil, false
}

// fitToWindow forgets the oldest messages as the window fills and puts the plan back in front of the model when forgetting left
// it too little to go on. It returns the conversation, grown by the plan when posted. The forgotten offset only moves forward.
func (e *Engine) fitToWindow(messages []conversation.Message, forgotten *int, turnStarts []int, tools []fantasy.Tool, emit func(Event)) []conversation.Message {
	if !e.forgetOldest(messages, forgotten, tools, emit) {
		return messages
	}

	turns := turnsHeld(turnStarts, *forgotten)

	if turns >= e.planTurns {
		return messages
	}

	posted, ok := e.repostedPlan(messages, *forgotten)
	if !ok {
		return messages
	}

	emit(Event{Kind: EventNotice, Text: fmt.Sprintf(
		"only %d turns are left in the context window; posting the plan again", turns)})

	messages = append(messages, posted...)

	// the plan costs something too
	e.forgetOldest(messages, forgotten, tools, emit)

	return messages
}

// trimmedKickoff stands in for the opening user message once trimming has
// dropped it. The goal lives in the instructions, so this only has to
// exist and point there.
const trimmedKickoff = "Continue working on your task as stated in the instructions."

// buildRequest assembles the provider request from what the window still holds.
// The conversation from the forgotten offset on.
func (e *Engine) buildRequest(messages []conversation.Message, forgotten int) turnRequest {
	chat := conversation.ToPrompt(messages[forgotten:])

	// Forgetting takes the oldest first, which is the opening user message. A conversation with no user
	// turn is rejected by strict providers, so one is restored. The goal itself is safe in the instructions.
	hasUser := false

	for _, message := range chat {
		if message.Role == fantasy.MessageRoleUser {
			hasUser = true

			break
		}
	}

	if !hasUser {
		chat = append(fantasy.Prompt{fantasy.NewUserMessage(trimmedKickoff)}, chat...)
	}

	// fantasy will not start a step from a conversation ending on the model's own words. The engine never
	// leaves one, but a handed-in conversation might, and one more line costs less than a run that cannot start.
	if last := chat[len(chat)-1]; last.Role != fantasy.MessageRoleUser && last.Role != fantasy.MessageRoleTool {
		chat = append(chat, fantasy.NewUserMessage(trimmedKickoff))
	}

	call := turnRequest{messages: chat}

	if e.options.MaxTokens != nil {
		call.maxOutput = new(int64(*e.options.MaxTokens))
	}

	return call
}

// toolDefinitions renders the tool schemas, with the terminal tools.
func (e *Engine) toolDefinitions() []fantasy.Tool {
	offered := slices.Concat(e.options.Tools, terminalTools())

	// Map order was random once, and a tool list that reshuffles between requests defeats server-side
	// prompt caches keyed on the prefix. So the order is fixed by name.
	slices.SortFunc(offered, func(a, b fantasy.AgentTool) int {
		return strings.Compare(a.Info().Name, b.Info().Name)
	})

	tools := make([]fantasy.Tool, 0, len(offered))

	for _, tool := range offered {
		info := tool.Info()

		inputSchema := map[string]any{
			"type":       "object",
			"properties": info.Parameters,
			"required":   info.Required,
		}

		schema.Normalize(inputSchema)

		tools = append(tools, fantasy.FunctionTool{
			Name:        info.Name,
			Description: info.Description,
			InputSchema: inputSchema,
		})
	}

	return tools
}

// Run drives the conversation to a conclusion, emitting events as it goes, and returns the Result. Watch sees each event as it
// happens and may be nil. Events are delivered synchronously, so a slow consumer throttles the run rather than dropping any.
func (e *Engine) Run(ctx context.Context, watch func(Event)) Result {
	emit := func(event Event) {
		if e.options.OnEvent != nil {
			e.options.OnEvent(event)
		}

		if watch != nil {
			watch(event)
		}
	}

	messages := append([]conversation.Message(nil), e.options.Messages...)

	budget := Budget{}

	// forgotten is how many of the oldest messages requests no longer carry. The
	// conversation itself keeps them all. Only the wire copy is short.
	forgotten := 0

	// turnStarts is where in the conversation each iteration began, and nudged is
	// the iteration the plan was last mentioned at.
	var turnStarts []int

	nudged := 0

	tools := e.toolDefinitions()

	state := &step{engine: e}
	agent := e.newAgent(state)

	started := time.Now()

	// Counts consecutive retriable provider failures for the backoff. Not budget.Continuations, which also
	// counts truncation recoveries and would escalate an unrelated outage. Both reset on a good turn.
	retries := 0

	// The latest provider failure, kept so an abort can carry it. A run is usually quit during a backoff
	// wait, and the top-of-loop cancellation check would otherwise discard the exchange being diagnosed.
	var lastFailure error

	for {
		if err := ctx.Err(); err != nil {
			return finish(messages, budget, StopAborted, "run canceled", firstNonNil(lastFailure, err))
		}

		// hand the conversation over before spending anything on the next turn,
		// so what a crash leaves behind is everything the run has actually done
		e.handOver(messages)

		// A time cap is checked at the iteration boundary like every budget. One long tool call can overrun by
		// one operation (the shell timeout bounds it), but no new iteration starts past the deadline.
		if e.maxDuration > 0 && time.Since(started) >= e.maxDuration {
			return finish(messages, budget, StopTime,
				fmt.Sprintf("stopped after %s", e.maxDuration), nil)
		}

		if budget.Iterations >= e.maxIterations {
			return finish(messages, budget, StopIterations,
				fmt.Sprintf("stopped after %d iterations", budget.Iterations), nil)
		}

		budget.Iterations++

		emit(Event{Kind: EventIteration, Iteration: budget.Iterations})

		if e.options.PlanTool != "" && e.planEvery > 0 && budget.Iterations%e.planEvery == 0 && nudged != budget.Iterations {
			nudged = budget.Iterations

			messages = append(messages, conversation.Message{Type: conversation.TypeUser, Text: planNudge(e.options.PlanTool)})
		}

		// a failed call is retried from the same place. It is one turn, not two
		if len(turnStarts) == 0 || turnStarts[len(turnStarts)-1] != len(messages) {
			turnStarts = append(turnStarts, len(messages))
		}

		messages = e.fitToWindow(messages, &forgotten, turnStarts, tools, emit)

		request := e.buildRequest(messages, forgotten)

		turn, err := e.runStep(ctx, agent, state, request, &messages, &budget, emit)

		// Accumulate the provider's reported usage, the actual billed tokens, and surface the running total so
		// a viewer shows real cost. Each call bills its whole prompt, so per-turn counts sum.
		if turn.InputTokens > 0 || turn.OutputTokens > 0 {
			budget.InputTokens += turn.InputTokens
			budget.OutputTokens += turn.OutputTokens

			emit(Event{
				Kind:         EventUsage,
				InputTokens:  budget.InputTokens,
				OutputTokens: budget.OutputTokens,
			})
		}

		if err != nil {
			// A failed model call is not an agentic round. Without this the round budget pays for every retry,
			// so twenty retries through an outage would cost twenty iterations. Continuations bound recovery.
			budget.Iterations--

			// Remember the failure so an abort during the backoff still carries it. Only a provider error, so a
			// bare cancellation does not overwrite the exchange worth keeping.
			if provider.IsProviderError(err) {
				lastFailure = err
			}

			// A cancellation mid-call surfaces here as the provider error it caused. It is recorded as the abort it
			// is, with the provider error carried along as evidence, so a quit during a failure keeps the dump.
			if ctx.Err() != nil {
				return finish(messages, budget, StopAborted, "run canceled", firstNonNil(lastFailure, err))
			}

			// a context-limit rejection is recoverable. Narrow the window the
			// thread is trimmed to and retry
			if limit, ok := provider.DetectContextLimit(err); ok && e.canContinue(budget) {
				budget.spendContinuation()

				if e.narrowWindow(limit, emit) {
					continue
				}
			}

			// A rate limit is recoverable, on the provider's schedule, which is why 429 is not IsRetriable.
			// Waiting out the advised delay is the other half. Without it one throttle response ends an overnight run.
			limited := provider.IsRateLimited(err)

			if (limited || provider.IsRetriable(err)) && e.canContinue(budget) {
				budget.spendContinuation()

				retries++

				emit(Event{Kind: EventRetry, Text: err.Error(), Failure: err})

				// Space the retries out. Otherwise the continuation budget is spent in milliseconds and the run dies
				// to an outage it would have outlived by waiting. Cancellation cuts the wait short.
				delay := backoffFor(e.retryBackoff, retries)

				if limited {
					// The advised delay is honored, but the backoff stays a floor under it, so "Retry-After: 0" cannot
					// become the instant-retry loop the backoff exists to prevent.
					advised, ok := provider.RetryAfter(err)
					delay = rateLimitWait(advised, ok, delay)
				}

				wait(ctx, delay)

				continue
			}

			return finish(messages, budget, StopError, "the provider failed", err)
		}

		// the provider answered. Whatever outage the backoff was pacing is over,
		// so the next one - if any - starts again from the base delay
		retries = 0

		// A turn that produced anything breaks the run of silences. The empty budget counts consecutive empty
		// turns, so scattered stalls must not add up to a StopEmpty hours later. Same reasoning as the cycle counter.
		if turn.Text != "" || turn.Reasoning != "" || len(turn.ToolCalls) > 0 {
			budget.Empties = 0
		}

		// record what the model produced - already done when the turn made tool
		// calls, which is when it has to be, ahead of them
		state.flush()

		// a truncated answer is continued rather than accepted

		if turn.FinishReason == fantasy.FinishReasonLength {
			if !e.canContinue(budget) {
				return finish(messages, budget, StopContinuations,
					"the model kept running out of output space", nil)
			}

			budget.spendContinuation()

			emit(Event{Kind: EventNotice, Text: fmt.Sprintf(
				"answer cut off at the output limit; asking the model to continue (%d/%d)",
				budget.Continuations, e.maxContinuations)})

			messages = append(messages, conversation.Message{Type: conversation.TypeUser, Text: truncationNotice()})

			continue
		}

		// The turn came back whole, so whatever the run was recovering from is behind it and the consecutive
		// count restarts. The total is untouched. It is the record of what the run spent, and the bound under this reset.
		if turn.Text != "" || turn.Reasoning != "" || len(turn.ToolCalls) > 0 {
			budget.Continuations = 0
		}

		// terminal tool calls end the run before anything else is dispatched

		if reason, detail, terminal := terminalCall(turn.ToolCalls); terminal {
			return finish(messages, budget, reason, detail, nil)
		}

		if len(turn.ToolCalls) > 0 {
			// the tools ran inside the step. What is left is the call budget
			if state.callsExhausted {
				return finish(messages, budget, StopCalls,
					fmt.Sprintf("stopped after %d tool calls", budget.Calls), nil)
			}

			// the loop only checks for repetition once tools have run, because
			// a repetition is a repetition of *actions*
			if next, stop := e.checkCycle(messages, &budget); stop != nil {
				return *stop
			} else if next != nil {
				messages = next
			}

			continue
		}

		// A really empty turn (no text, no reasoning, no tool call) is a stuck model, bounded tightly by the
		// empty budget, not the larger settle budget. The nudge still points at the terminal tools.

		if turn.Text == "" && turn.Reasoning == "" {
			if budget.Empties >= e.maxEmpties {
				return finish(messages, budget, StopEmpty,
					"the model repeatedly produced nothing", nil)
			}

			budget.Empties++

			// Visible, because a stalling provider otherwise renders as bare iteration dividers, and a run being
			// nudged back to life looked exactly like a hang.
			emit(Event{Kind: EventNotice, Text: fmt.Sprintf(
				"the model returned an empty turn; nudging it to continue (%d/%d)",
				budget.Empties, e.maxEmpties)})

			messages = append(messages, conversation.Message{Type: conversation.TypeUser, Text: settleNotice()})

			continue
		}

		// The model produced content but did not act. That is not an ending. Nudge
		// it toward success / failure, up to maxSettles.

		if budget.Settles >= e.maxSettles {
			return finish(messages, budget, StopUnsettled,
				"the model stopped without recording an outcome", nil)
		}

		budget.Settles++

		emit(Event{Kind: EventNotice, Text: fmt.Sprintf(
			"the model stopped without recording an outcome; nudging it to settle (%d/%d)",
			budget.Settles, e.maxSettles)})

		messages = append(messages, conversation.Message{Type: conversation.TypeUser, Text: settleNotice()})
	}
}

// activityMessage renders one half of a tool-call pair.
func activityMessage(kind conversation.ActivityKind, call fantasy.ToolCallContent, result any, failure string) conversation.Message {
	activity := &conversation.Activity{
		Kind:      kind,
		ID:        call.ToolCallID,
		Name:      call.ToolName,
		Arguments: call.Input,
	}

	if kind == conversation.ActivityResponse {
		activity.Result = result
		activity.Failure = failure
	}

	// the text is what the model is shown. A request shows nothing, because the
	// call itself is carried in the wire format's tool_calls field
	return conversation.Message{
		Type:     conversation.TypeActivity,
		Text:     activity.ResultText(),
		Activity: activity,
	}
}
