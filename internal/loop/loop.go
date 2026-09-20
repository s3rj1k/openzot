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

	// Unrepaired names the tools whose calls are never repaired. fantasy mends a
	// call whose input does not parse - a missing brace, an unterminated string,
	// a stray comma - so a small slip costs the model no turn. For a tool that
	// acts on the machine that is the wrong trade: the mended input is one the
	// model never finished writing. A call to one of these goes back to the model
	// as it stands, with the reason.
	Unrepaired []string

	// OnConversation, when set, is called at each iteration boundary, and again
	// just before each tool handler runs, with the conversation as it then
	// stands. It exists so a caller can persist the conversation as the run goes
	// rather than only when it ends - the whole point of a session log is that a
	// run killed at iteration 500 still leaves its record, which it does not if
	// nothing was written down until iteration 500 finished.
	//
	// The slice handed over is the whole conversation as it then stands, not a
	// delta. The engine only ever appends to it - what is sent on the wire is
	// trimmed to the window, but the conversation itself is never rewritten.
	OnConversation func([]conversation.Message)

	// OnEvent, when set, sees every event alongside the function Run is given.
	// It is for a sink that has to see the whole run whoever is watching it - a
	// session log - where the function Run is given belongs to the caller
	// showing the run.
	OnEvent func(Event)

	// MaxIterations, MaxContinuations, MaxCycles, MaxEmpties bound the run. Zero
	// uses the corresponding default.
	MaxIterations    int
	MaxContinuations int
	MaxCycles        int
	MaxEmpties       int

	// MaxRecoveries bounds recovery attempts across the whole run, however they
	// are spaced - the point at which a provider that keeps needing them is
	// called broken. See DefaultMaxRecoveries for why it is separate from
	// MaxContinuations rather than derived from it. Zero uses the default.
	MaxRecoveries int

	// MaxCalls bounds total tool calls. Zero (or negative) is unbounded - only
	// the iteration count is a hard default backstop.
	MaxCalls int

	// MaxDuration bounds the wall-clock time of a run, checked at each iteration
	// boundary. Zero is unbounded.
	MaxDuration time.Duration

	// RetryBackoff is the pause before the first retry of a retriable provider
	// failure, doubling per consecutive retry up to MaxRetryBackoff. Zero uses
	// DefaultRetryBackoff; negative disables the wait entirely, which only a test
	// driving an outage should ask for.
	RetryBackoff time.Duration

	// MaxSettles bounds how many times the model is nudged to record an outcome
	// before the run is surfaced as unsettled. Zero uses the default. There is no
	// way to turn settling off: a run finishes only when the model calls a
	// terminal tool, which is what replaces deciding a task is done because the
	// answer contained the word "completed".
	MaxSettles int

	// MaxTokens bounds a single response.
	MaxTokens *int

	// PlanTool names the tool the model keeps its plan with. Empty turns the plan
	// handling off: nothing is nudged or posted. The engine does not know the
	// tool's schema - the plan is simply its latest successful call.
	PlanTool string

	// PlanNudgeEvery is how many iterations pass between reminders of the plan
	// tool. Zero uses DefaultPlanNudgeEvery; negative turns the reminders off.
	PlanNudgeEvery int

	// PlanMinTurns is the number of turns the window must still hold after
	// forgetting for the plan to be left where it is; fewer and it is posted
	// again. Zero uses DefaultPlanMinTurns.
	PlanMinTurns int

	// ContextSoft and ContextHard are the percentages of the window at which the
	// oldest messages start to be forgotten (one per request) and at which as
	// many as it takes are (so the request stays under it). Zero uses
	// DefaultContextSoft and DefaultContextHard.
	ContextSoft int
	ContextHard int

	// ContextWindow overrides the model's total context window, in tokens.
	// Required: New refuses a run without it. There is no built-in table of what
	// each model can take - the operator states it, because only the operator
	// knows the real ceiling of the endpoint being served.
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

// ExitCode is the process-style exit code the run's ending maps onto.
//
// Zero means the run reached a conclusion it stands behind and that conclusion was
// success - the model settled, or finished talking in a run that does not require
// settling. Everything else means the task did not get done: either the model
// declared it could not be done (StopFailed) or the run was cut short by a guard.
// A caller scripting against zot needs to tell those apart from success without
// parsing prose.
func (r Result) ExitCode() int {
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

	// maxRecoveries is the runaway backstop under the consecutive count; see
	// DefaultMaxRecoveries.
	maxRecoveries int
	maxCycles     int
	maxEmpties    int
	maxSettles    int
	retryBackoff  time.Duration

	// window is the context window requests are held under: the configured one,
	// lowered when a provider rejects a request and states its own ceiling.
	window      int
	planEvery   int
	planTurns   int
	softPercent int
	hardPercent int

	// toolTokens caches the cost of the tool schemas, which are identical on
	// every request of a run and would otherwise be re-counted each round.
	toolTokens int
}

// toolSchemaTokens is what the tool definitions cost on the wire.
//
// They are sent with every request and can be substantial - a dozen tools with
// detailed JSON Schema runs to thousands of tokens - so leaving them out of the
// budget is how a thread that "fits" gets rejected.
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
func New(options Options) (*Engine, error) {
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
		options:       options,
		maxIterations: pick(options.MaxIterations, DefaultMaxIterations),
		// @note calls and time are unbounded unless the caller sets them: only
		// the iteration count is a hard default backstop. A non-positive value
		// means "no cap", which is why they are stored raw rather than picked.
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

// canContinue reports whether another recovery attempt is within both bounds:
// the consecutive run of them, and the total across the run. Both are checked
// at every site that spends one, so neither can be reached by taking a
// different route into recovery.
func (e *Engine) canContinue(budget Budget) bool {
	return budget.Continuations < e.maxContinuations &&
		budget.Recoveries < e.maxRecoveries
}

// backoffFor is the pause before the attempt'th consecutive retry: base,
// doubling per attempt, capped at MaxRetryBackoff - including a base that
// already exceeds the cap, so a generous RetryBackoff cannot make the first
// retry the longest wait of the run. A non-positive base means the caller asked
// for no wait at all.
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

// rateLimitWait is how long to sit out a rate limit: the larger of the delay
// the provider advised and the ordinary backoff for this retry.
//
// The backoff is a floor, not just a fallback. A provider that keeps answering
// 429 with "Retry-After: 0" (or a date already past) would otherwise be
// hammered with instant retries - the exact tight loop the backoff exists to
// prevent - while a genuine large advice still wins over a small backoff.
//
// The advice is capped. A run that waits out a real rate-limit window is doing
// the right thing, but an unattended run must not be parked for hours by a
// mistaken or hostile header, and no legitimate window needs longer than the cap.
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

// wait pauses for d, or until the run is cancelled - whichever comes first. It
// does not report which: the caller loops back to the cancellation check at the
// top of Run, so a cancelled wait ends the run there rather than in two places.
func (e *Engine) wait(ctx context.Context, d time.Duration) {
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

// Run drives the conversation to a conclusion, emitting events as it goes.
//
// Run returns the Result; watch sees each event as it happens, and a nil watch is
// allowed. Events are delivered synchronously on the calling goroutine, so a
// slow consumer throttles the run rather than dropping anything.
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
	// conversation itself keeps them all; only the wire copy is short.
	forgotten := 0

	// turnStarts is where in the conversation each iteration began, and nudged is
	// the iteration the plan was last mentioned at.
	var turnStarts []int

	nudged := 0

	tools := e.toolDefinitions()

	state := &step{engine: e}
	agent := e.newAgent(state)

	started := time.Now()

	// retries counts *consecutive* retriable provider failures, and is what the
	// backoff keys off. Deliberately not budget.Continuations: that also counts
	// truncation recoveries and context-limit retries, so keying the
	// backoff off it would make a truncation earlier in the same stretch start
	// an unrelated outage at an escalated wait. Both reset on a good turn.
	retries := 0

	// The most recent provider failure, kept so an abort can carry it. A run is
	// usually quit during a backoff wait, not during the failing call itself,
	// so the top-of-loop cancellation check would otherwise discard the very
	// exchange being diagnosed - the failure whose dump the operator quit to
	// go and read.
	var lastFailure error

	for {
		if err := ctx.Err(); err != nil {
			return e.finish(messages, budget, StopAborted, "run cancelled", firstNonNil(lastFailure, err))
		}

		// hand the conversation over before spending anything on the next turn,
		// so what a crash leaves behind is everything the run has actually done
		e.handOver(messages)

		// A time cap is checked at the iteration boundary, like every other
		// budget. A single long tool call can overrun by one operation - the
		// shell tool's own timeout bounds that - but the run will not start
		// another iteration past the deadline.
		if e.maxDuration > 0 && time.Since(started) >= e.maxDuration {
			return e.finish(messages, budget, StopTime,
				fmt.Sprintf("stopped after %s", e.maxDuration), nil)
		}

		if budget.Iterations >= e.maxIterations {
			return e.finish(messages, budget, StopIterations,
				fmt.Sprintf("stopped after %d iterations", budget.Iterations), nil)
		}

		budget.Iterations++

		emit(Event{Kind: EventIteration, Iteration: budget.Iterations})

		if e.options.PlanTool != "" && e.planEvery > 0 && budget.Iterations%e.planEvery == 0 && nudged != budget.Iterations {
			nudged = budget.Iterations

			messages = append(messages, conversation.Message{Type: conversation.TypeUser, Text: planNudge(e.options.PlanTool)})
		}

		// a failed call is retried from the same place: it is one turn, not two
		if len(turnStarts) == 0 || turnStarts[len(turnStarts)-1] != len(messages) {
			turnStarts = append(turnStarts, len(messages))
		}

		messages = e.fitToWindow(messages, &forgotten, turnStarts, tools, emit)

		request := e.buildRequest(messages, forgotten)

		turn, err := e.runStep(ctx, agent, state, request, &messages, &budget, emit)

		// Accumulate the provider's reported usage - the actual billed tokens - and
		// surface the running total so a viewer can show real cost rather than an
		// estimate. Each call bills its whole prompt, so the per-turn counts sum.
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
			// A failed model call is not an agentic round: the recovery paths
			// below hand the failure back to the top of the loop, and without
			// this the round budget pays for every retry - twenty spaced
			// retries through an outage would cost twenty iterations of work
			// the run never got. Continuations are the bound on recovery
			// attempts; iterations count normal progress.
			budget.Iterations--

			// remember the failure so an abort during the ensuing backoff still
			// carries it - only a provider error, so a bare cancellation does
			// not overwrite the exchange worth keeping
			if provider.IsProviderError(err) {
				lastFailure = err
			}

			// A cancellation that lands mid-call surfaces here as the provider
			// error it caused - a cut stream, an aborted request - rather than
			// at the top-of-loop check. It is recorded as the abort it is - a
			// user quitting is not a provider failing - but the provider error
			// is carried along as the evidence, so a quit during a provider
			// failure still preserves the failing exchange (and its dump)
			// rather than discarding the very thing being diagnosed.
			if ctx.Err() != nil {
				return e.finish(messages, budget, StopAborted, "run cancelled", firstNonNil(lastFailure, err))
			}

			// a context-limit rejection is recoverable: narrow the window the
			// thread is trimmed to and retry
			if limit, ok := provider.DetectContextLimit(err); ok && e.canContinue(budget) {
				budget.spendContinuation()

				if e.narrowWindow(limit, emit) {
					continue
				}
			}

			// A rate limit is recoverable, but on the provider's schedule rather
			// than ours - which is why 429 is not IsRetriable. Waiting out the
			// advised delay is the other half of that contract; without it a
			// single throttle response ends an overnight run outright.
			limited := provider.IsRateLimited(err)

			if (limited || provider.IsRetriable(err)) && e.canContinue(budget) {
				budget.spendContinuation()
				retries++

				emit(Event{Kind: EventRetry, Text: err.Error(), Failure: err})

				// Space the retries out. Without this the continuation budget is
				// spent in milliseconds, so a run dies to an outage it would have
				// outlived by waiting - and the retries land on an endpoint that
				// is already failing. Cancellation cuts the wait short; the check
				// at the top of the loop then ends the run.
				delay := backoffFor(e.retryBackoff, retries)

				if limited {
					// the advised delay is honoured, but the backoff stays a
					// floor under it - "Retry-After: 0" must not turn into the
					// instant-retry loop the backoff exists to prevent
					advised, ok := provider.RetryAfter(err)
					delay = rateLimitWait(advised, ok, delay)
				}

				e.wait(ctx, delay)

				continue
			}

			return e.finish(messages, budget, StopError, "the provider failed", err)
		}

		// the provider answered: whatever outage the backoff was pacing is over,
		// so the next one - if any - starts again from the base delay
		retries = 0

		// a turn that produced *anything* breaks the run of silences: the empty
		// budget counts CONSECUTIVE empty turns, so single stalls scattered over a
		// long run must not add up to a StopEmpty hours later. Same reasoning as
		// the cycle counter, which zeroes the moment a round comes back clean.
		if turn.Text != "" || turn.Reasoning != "" || len(turn.ToolCalls) > 0 {
			budget.Empties = 0
		}

		// record what the model produced - already done when the turn made tool
		// calls, which is when it has to be, ahead of them
		state.flush()

		// a truncated answer is continued rather than accepted

		if turn.FinishReason == fantasy.FinishReasonLength {
			if !e.canContinue(budget) {
				return e.finish(messages, budget, StopContinuations,
					"the model kept running out of output space", nil)
			}

			budget.spendContinuation()

			emit(Event{Kind: EventNotice, Text: fmt.Sprintf(
				"answer cut off at the output limit; asking the model to continue (%d/%d)",
				budget.Continuations, e.maxContinuations)})

			messages = append(messages, conversation.Message{Type: conversation.TypeUser, Text: truncationNotice()})

			continue
		}

		// The turn came back whole - not a provider failure, not cut off at the
		// output limit - so whatever the run was recovering from is behind it
		// and the consecutive count starts over. Everything that needs recovery
		// continues above this line, which is what keeps a run of truncations
		// bounded while a truncation an hour ago no longer counts against a
		// provider blip now. The total is untouched: it is the record of what
		// the run spent, and the backstop under this reset.
		if turn.Text != "" || turn.Reasoning != "" || len(turn.ToolCalls) > 0 {
			budget.Continuations = 0
		}

		// terminal tool calls end the run before anything else is dispatched

		if reason, detail, terminal := e.terminalCall(turn.ToolCalls); terminal {
			return e.finish(messages, budget, reason, detail, nil)
		}

		if len(turn.ToolCalls) > 0 {
			// the tools ran inside the step; what is left is the call budget
			if state.callsExhausted {
				return e.finish(messages, budget, StopCalls,
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

		// A genuinely empty turn - no text, no reasoning, no tool call - is a stuck
		// model, bounded tightly by the empty budget: a run producing nothing must
		// not burn the whole (much larger) settle budget on silence. The nudge still
		// points at the terminal tools, though - a plain "say you are finished"
		// would not record the outcome a run requires.

		if turn.Text == "" && turn.Reasoning == "" {
			if budget.Empties >= e.maxEmpties {
				return e.finish(messages, budget, StopEmpty,
					"the model repeatedly produced nothing", nil)
			}

			budget.Empties++

			// visible, because a stuck or stalling provider otherwise renders as
			// bare iteration dividers with nothing between them - a run being
			// nudged back to life looked exactly like a hang
			emit(Event{Kind: EventNotice, Text: fmt.Sprintf(
				"the model returned an empty turn; nudging it to continue (%d/%d)",
				budget.Empties, e.maxEmpties)})

			messages = append(messages, conversation.Message{Type: conversation.TypeUser, Text: settleNotice()})

			continue
		}

		// The model produced content but did not act. That is not an ending: nudge
		// it toward success / failure, up to maxSettles.

		if budget.Settles >= e.maxSettles {
			return e.finish(messages, budget, StopUnsettled,
				"the model stopped without recording an outcome", nil)
		}

		budget.Settles++

		emit(Event{Kind: EventNotice, Text: fmt.Sprintf(
			"the model stopped without recording an outcome; nudging it to settle (%d/%d)",
			budget.Settles, e.maxSettles)})

		messages = append(messages, conversation.Message{Type: conversation.TypeUser, Text: settleNotice()})
	}
}

// terminalCall reports whether the model ended the run with a terminal tool.
func (e *Engine) terminalCall(calls []fantasy.ToolCallContent) (StopReason, string, bool) {
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

// terminalDetail pulls the explanation out of a terminal call's arguments.
func terminalDetail(call fantasy.ToolCallContent, key, fallback string) string {
	if value, ok := decodeInput(call.Input)[key].(string); ok && value != "" {
		return value
	}

	return fallback
}

// handOver gives the conversation as it stands to the OnConversation hook.
func (e *Engine) handOver(messages []conversation.Message) {
	if e.options.OnConversation != nil {
		e.options.OnConversation(messages)
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

	// the text is what the model is shown; a request shows nothing, because the
	// call itself is carried in the wire format's tool_calls field
	return conversation.Message{
		Type:     conversation.TypeActivity,
		Text:     activity.ResultText(),
		Activity: activity,
	}
}

// checkCycle looks for repetition and nudges the model, or stops the run once
// nudging has failed enough times.
func (e *Engine) checkCycle(messages []conversation.Message, budget *Budget) ([]conversation.Message, *Result) {
	detected := describeCycle(messages)

	if detected == "" {
		// a round that is not cyclic breaks the run of repetitions: the budget
		// counts *consecutive* cycles, so two unrelated repetitions far apart in a
		// long run must not add up to a stop. This mirrors the source, which zeroes
		// its cycle counter the moment a round comes back clean.
		budget.Cycles = 0

		return nil, nil
	}

	if budget.Cycles >= e.maxCycles {
		result := e.finish(messages, *budget, StopCycle,
			fmt.Sprintf("the model kept repeating itself (%s)", detected), nil)

		return nil, &result
	}

	budget.Cycles++

	return append(messages, conversation.Message{Type: conversation.TypeUser, Text: cycleNotice(cycleDetail(detected))}), nil
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

// narrowWindow lowers the context window requests are held under, after a
// provider rejected a request as too long. It reports whether the window went
// down - if not there is nothing left to try, and the rejection is a real
// failure.
//
// The provider's stated window beats the configured one. A rejection is
// precisely the case where the configured window was wrong - a serving endpoint
// with a smaller ceiling than the operator stated - so believing the error is
// what makes the retry fit instead of guessing again. A rejection that states no
// window, or one no lower than the window already in force, still has to shrink
// something or the retry would send the identical request: the window steps down
// by a quarter instead, until it reaches a fraction of the configured one.
//
// Only the window changes. The conversation itself is untouched; the oldest
// messages are forgotten to fit it on the next request.
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

// fitToWindow forgets the oldest messages as the window fills, and puts the plan
// back in front of the model when forgetting has left it with too little to go
// on. It returns the conversation, which has grown by the plan when that was
// posted. forgotten is the run's offset into messages and only moves forward.
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

// repostedPlan is the model's latest plan - its last successful call of the plan
// tool - as a fresh call and result to append to the conversation. It reports
// false when there is no plan, or when the plan is still in the window and so
// needs no help.
func (e *Engine) repostedPlan(messages []conversation.Message, forgotten int) ([]conversation.Message, bool) {
	if e.options.PlanTool == "" {
		return nil, false
	}

	for index := len(messages) - 1; index >= 0; index-- {
		activity := messages[index].Activity

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

// buildRequest assembles the provider request from what the window still holds:
// the conversation from the forgotten offset on.
func (e *Engine) buildRequest(messages []conversation.Message, forgotten int) turnRequest {
	chat := conversation.ToPrompt(messages[forgotten:])

	// Forgetting takes the oldest first, which is the run's opening user message.
	// A conversation with no user turn at all is invalid to strict providers:
	// they reject the whole request, deterministically, from that iteration on
	// (bisected live against one that answered only an opaque 400). The
	// objective itself is safe in the instructions; what must be restored is a
	// user turn's existence.
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

	// fantasy will not start a step from a conversation that ends on the model's
	// own words. The engine never leaves one - every turn is followed by a tool
	// result or a nudge - but a conversation it was handed might, and one more
	// line to continue costs less than a run that cannot start.
	if last := chat[len(chat)-1]; last.Role != fantasy.MessageRoleUser && last.Role != fantasy.MessageRoleTool {
		chat = append(chat, fantasy.NewUserMessage(trimmedKickoff))
	}

	call := turnRequest{messages: chat}

	if e.options.MaxTokens != nil {
		limit := int64(*e.options.MaxTokens)

		call.maxOutput = &limit
	}

	return call
}

// trimmedKickoff stands in for the opening user message once trimming has
// dropped it. The objective lives in the instructions, so this only has to
// exist and point there.
const trimmedKickoff = "Continue working on your task as stated in the instructions."

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

// toolDefinitions renders the tool schemas, with the terminal tools.
func (e *Engine) toolDefinitions() []fantasy.Tool {
	var offered []fantasy.AgentTool

	offered = append(offered, e.options.Tools...)
	offered = append(offered, terminalTools()...)

	// map order was random once and a tool list that reshuffles between requests
	// defeats any server-side prompt cache keyed on the prefix, so the order is
	// fixed: by name
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

func (e *Engine) finish(messages []conversation.Message, budget Budget, reason StopReason, detail string, err error) Result {
	return Result{
		Reason:   reason,
		Message:  detail,
		Messages: messages,
		Budget:   budget,
		Err:      err,
	}
}
