package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/schema"

	"github.com/openzot/openzot/internal/thread"
)

// Options configures a run.
type Options struct {
	// Client is the provider connection.
	Client *Client

	// Instructions is the system prompt.
	Instructions string

	// Messages seeds the conversation.
	Messages []Message

	// Tools the model may call.
	Tools []fantasy.AgentTool

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
	OnConversation func([]Message)

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

	// MaxSettles enables settle mode when positive: the run finishes only when
	// the model calls a terminal tool. This is what replaces deciding a task is
	// done because the answer contained the word "completed".
	MaxSettles int

	// MaxTokens bounds a single response.
	MaxTokens *int

	// LimitCheckpoints are the percentages of a bounded limit at which the model
	// is told it is approaching that limit. Nil uses DefaultLimitCheckpoints; a
	// non-nil empty slice disables the notices.
	LimitCheckpoints []int

	// ContextWindow overrides the model's total context window, in tokens.
	// Required: New refuses a run without it. There is no built-in table of what
	// each model can take - the operator states it, because only the operator
	// knows the real ceiling of the endpoint being served.
	ContextWindow int
}

// MessageType identifies what a message is.
//
// A named type rather than a bare string: these values decide how a message is
// rendered to the provider and whether the runaway backstop scans it. A typo in
// a string literal would silently route a message down the wrong path - a system
// prompt rendered as ordinary history, say - and nothing would report it.
type MessageType string

// The message types the loop understands.
const (
	// TypeUser is input from the operator, and the channel the loop injects its
	// own notices on.
	TypeUser MessageType = "user"

	// TypeBot is the model's answer.
	TypeBot MessageType = "bot"

	// TypeReasoning is the model's scratchpad. Never replayed to the provider,
	// and exempt from the runaway-text backstop.
	TypeReasoning MessageType = "reasoning"

	// TypeActivity is one half of a tool-call pair.
	TypeActivity MessageType = "activity"

	// TypeInstructions is system context - the instructions that shape the run.
	// Always ordered ahead of everything else.
	TypeInstructions MessageType = "instructions"
)

// Message is one entry in the conversation.
type Message struct {
	Type MessageType `json:"type"`
	Text string      `json:"text"`

	// Activity is the tool call this message carries, on a TypeActivity
	// message. Nil on every other type.
	Activity *Activity `json:"activity,omitempty"`
}

// Result is the outcome of a run.
type Result struct {
	// Reason is why the run ended.
	Reason StopReason

	// Message is a human-readable explanation.
	Message string

	// Messages is the conversation as it ended.
	Messages []Message

	// Budget is what the run spent.
	Budget Budget

	// Err is set when Reason is StopError.
	Err error
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
	checkpoints   []int

	inputBudget int

	// toolTokens caches the cost of the tool schemas, which are identical on
	// every request of a run and would otherwise be re-counted each round.
	toolTokens int

	// tools is Options.Tools by name, for dispatch.
	tools map[string]fantasy.AgentTool
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

	e.toolTokens = estimateTokens(string(encoded))

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

	// Three quarters of the window is input; the rest is the room the answer
	// needs. Not all of it: a request that fills the window leaves the model
	// nowhere to write.
	budget := options.ContextWindow - options.ContextWindow/4

	if budget < MinInputTokens {
		budget = MinInputTokens
	}

	tools := make(map[string]fantasy.AgentTool, len(options.Tools))

	for _, tool := range options.Tools {
		tools[tool.Info().Name] = tool
	}

	return &Engine{
		options:       options,
		tools:         tools,
		maxIterations: pick(options.MaxIterations, DefaultMaxIterations),
		// @note calls and time are unbounded unless the caller sets them: only
		// the iteration count is a hard default backstop. A non-positive value
		// means "no cap", which is why they are stored raw rather than picked.
		maxCalls:         nonNegative(options.MaxCalls),
		maxDuration:      options.MaxDuration,
		maxContinuations: pick(options.MaxContinuations, DefaultMaxContinuations),
		maxRecoveries:    pick(options.MaxRecoveries, DefaultMaxRecoveries),
		maxCycles:        pick(options.MaxCycles, DefaultMaxCycles),
		maxEmpties:       pick(options.MaxEmpties, DefaultMaxEmpties),
		maxSettles:       options.MaxSettles,
		checkpoints:      normalizeCheckpoints(options.LimitCheckpoints),
		// @note negative means "no wait" and is stored raw, so a test driving an
		// outage does not have to sleep through it. Zero takes the default.
		retryBackoff: pickDuration(options.RetryBackoff, DefaultRetryBackoff),
		inputBudget:  budget,
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

// pickDuration returns value when it is set, and the fallback when it is zero.
// A negative value is kept, meaning "explicitly none".
func pickDuration(value, fallback time.Duration) time.Duration {
	if value == 0 {
		return fallback
	}

	return value
}

// nonNegative clamps a budget to zero, so a negative value means the same as
// unset - unbounded - rather than an ever-true stop condition.
// firstNonNil returns a if it is set, else b - so an abort prefers the
// provider failure worth diagnosing over the bare cancellation.
func firstNonNil(a, b error) error {
	if a != nil {
		return a
	}

	return b
}

func nonNegative(v int) int {
	if v < 0 {
		return 0
	}

	return v
}

// noteApproachingLimits appends an approaching-limit notice for each configured
// checkpoint a bounded limit has newly crossed. Only bounded limits are checked:
// an unbounded call or time budget has nothing to approach, and a checkpoint
// fires at most once because its per-limit mark only advances.
func (e *Engine) noteApproachingLimits(messages []Message, budget *Budget, elapsed time.Duration) []Message {
	if len(e.checkpoints) == 0 {
		return messages
	}

	cross := func(mark *int, used, max int, kind limitKind, usage string) {
		if max <= 0 {
			return
		}

		pct := used * 100 / max

		for *mark < len(e.checkpoints) && pct >= e.checkpoints[*mark] {
			hit := e.checkpoints[*mark]
			*mark++

			messages = append(messages, Message{
				Type: TypeUser,
				Text: limitCheckpointNotice(kind, hit, usage),
			})
		}
	}

	cross(&budget.iterCheckpoint, budget.Iterations, e.maxIterations, iterationLimit,
		fmt.Sprintf("%d of %d", budget.Iterations, e.maxIterations))

	if e.maxCalls > 0 {
		cross(&budget.callCheckpoint, budget.Calls, e.maxCalls, toolCallLimit,
			fmt.Sprintf("%d of %d", budget.Calls, e.maxCalls))
	}

	if e.maxDuration > 0 {
		cross(&budget.timeCheckpoint, int(elapsed.Seconds()), int(e.maxDuration.Seconds()), timeLimit,
			fmt.Sprintf("%s of %s", elapsed.Round(time.Second), e.maxDuration))
	}

	return messages
}

// settleMode reports whether the run ends only on a terminal tool call.
func (e *Engine) settleMode() bool {
	return e.maxSettles > 0
}

// Run drives the conversation to a conclusion, emitting events as it goes.
//
// Run returns the Result; emit sees each event as it happens, and a nil emit is
// allowed. Events are delivered synchronously on the calling goroutine, so a
// slow consumer throttles the run rather than dropping anything.
func (e *Engine) Run(ctx context.Context, emit func(Event)) Result {
	if emit == nil {
		emit = func(Event) {}
	}

	messages := append([]Message(nil), e.options.Messages...)

	budget := Budget{}

	tools := e.toolDefinitions()

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

		// Tell the model when it crosses a checkpoint of a bounded limit, so it
		// can pace itself and finish before the hard stop rather than being cut
		// off mid-task. The notice reflects consumption so far - iterations now,
		// tool calls and time from the rounds already done.
		messages = e.noteApproachingLimits(messages, &budget, time.Since(started))

		request, err := e.buildRequest(messages, tools)
		if err != nil {
			return e.finish(messages, budget, StopError, "could not assemble the request", err)
		}

		turn, err := e.runTurn(ctx, request, emit)

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
			if IsProviderError(err) {
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
			if limit, ok := DetectContextLimit(err); ok && e.canContinue(budget) {
				budget.spendContinuation()

				if e.narrowInputBudget(limit, emit) {
					continue
				}
			}

			// A rate limit is recoverable, but on the provider's schedule rather
			// than ours - which is why 429 is not IsRetriable. Waiting out the
			// advised delay is the other half of that contract; without it a
			// single throttle response ends an overnight run outright.
			limited := IsRateLimited(err)

			if (limited || IsRetriable(err)) && e.canContinue(budget) {
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
					advised, ok := RetryAfter(err)
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

		// record what the model produced

		if turn.Reasoning != "" {
			messages = append(messages, Message{Type: TypeReasoning, Text: turn.Reasoning})

			emit(Event{Kind: EventMessage, MessageType: TypeReasoning, Text: turn.Reasoning})
		}

		if turn.Text != "" {
			messages = append(messages, Message{Type: TypeBot, Text: turn.Text})

			emit(Event{Kind: EventMessage, MessageType: TypeBot, Text: turn.Text})
		}

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

			messages = append(messages, Message{Type: TypeUser, Text: truncationNotice()})

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
			var stop *Result

			messages, stop = e.dispatch(ctx, messages, turn.ToolCalls, &budget, emit)

			if stop != nil {
				return *stop
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
		// model, bounded tightly by the empty budget in either mode: a run producing
		// nothing must not burn the whole (much larger) settle budget on silence. In
		// settle mode the nudge still points at the terminal tools, though - a plain
		// "say you are finished" would not record the outcome settle mode requires.

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

			nudge := emptyNotice()
			if e.settleMode() {
				nudge = settleNotice()
			}

			messages = append(messages, Message{Type: TypeUser, Text: nudge})

			continue
		}

		// The model produced content but did not act. In settle mode that is not an
		// ending: nudge it toward success / failure, up to maxSettles.

		if e.settleMode() {
			if budget.Settles >= e.maxSettles {
				return e.finish(messages, budget, StopUnsettled,
					"the model stopped without recording an outcome", nil)
			}

			budget.Settles++

			emit(Event{Kind: EventNotice, Text: fmt.Sprintf(
				"the model stopped without recording an outcome; nudging it to settle (%d/%d)",
				budget.Settles, e.maxSettles)})

			messages = append(messages, Message{Type: TypeUser, Text: settleNotice()})

			continue
		}

		return e.finish(messages, budget, StopStop, turn.Text, nil)
	}
}

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

// runTurn performs a single model call, streaming its output through emit and
// watching for a runaway.
func (e *Engine) runTurn(ctx context.Context, call fantasy.Call, emit func(Event)) (turnResult, error) {
	// A turn can end while the provider is still streaming - the runaway guard
	// cuts a degenerate one short - so every exit from here cancels the stream,
	// which closes the response body rather than leaving it open for the life
	// of the process.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		text      strings.Builder
		reasoning strings.Builder
		result    turnResult
	)

	minChars := RunawayGuardMinChars

	guard := thread.NewGuard(thread.GuardOptions{MinChars: &minChars})

	for part := range e.options.Client.Stream(ctx, call) {
		switch part.Type {
		case fantasy.StreamPartTypeError:
			return turnResult{}, part.Error

		case fantasy.StreamPartTypeTextDelta:
			text.WriteString(part.Delta)

			emit(Event{Kind: EventToken, Text: part.Delta})

			// the streaming guard cuts a degenerate turn short rather than
			// letting it burn the whole output budget
			if guard.Push(part.Delta) {
				result.Text = text.String()
				result.Reasoning = reasoning.String()
				result.FinishReason = fantasy.FinishReasonStop

				if reason := guard.Reason(); reason != nil {
					emit(Event{Kind: EventRunaway, Text: reason.Text})
				}

				return result, nil
			}

		case fantasy.StreamPartTypeReasoningDelta:
			reasoning.WriteString(part.Delta)

			emit(Event{Kind: EventReasoningToken, Text: part.Delta})

		case fantasy.StreamPartTypeToolCall:
			result.ToolCalls = append(result.ToolCalls, fantasy.ToolCallContent{
				ToolCallID: part.ID,
				ToolName:   part.ToolCallName,
				Input:      part.ToolCallInput,
			})

		case fantasy.StreamPartTypeFinish:
			result.FinishReason = part.FinishReason

			// the provider's own count, which reflects what it actually processed
			// (server-side prompt caching and all) - never the local estimate
			// fantasy reports the prompt without its cached part; zot has always
			// counted the whole prompt, so the cached tokens are added back
			if prompt := part.Usage.InputTokens + part.Usage.CacheReadTokens; prompt > 0 {
				result.InputTokens = int(prompt)
			}

			result.OutputTokens = int(part.Usage.OutputTokens)
		}
	}

	result.Text = text.String()
	result.Reasoning = reasoning.String()

	return result, nil
}

// terminalCall reports whether the model ended the run with a terminal tool.
func (e *Engine) terminalCall(calls []fantasy.ToolCallContent) (StopReason, string, bool) {
	if !e.settleMode() {
		return "", "", false
	}

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
	arguments, err := decodeArguments(call)
	if err != nil {
		return fallback
	}

	if value, ok := arguments[key].(string); ok && value != "" {
		return value
	}

	return fallback
}

// dispatch executes the tools the model requested and appends the results.
//
// Each call becomes a request/response activity pair. The pairing is not
// cosmetic: a provider rejects a turn whose tool result has no matching call, so
// the two must be appended together and must survive trimming together.
func (e *Engine) dispatch(
	ctx context.Context,
	messages []Message,
	calls []fantasy.ToolCallContent,
	budget *Budget,
	emit func(Event),
) ([]Message, *Result) {
	for _, call := range calls {
		if e.maxCalls > 0 && budget.Calls >= e.maxCalls {
			result := e.finish(messages, *budget, StopCalls,
				fmt.Sprintf("stopped after %d tool calls", budget.Calls), nil)

			return messages, &result
		}

		budget.Calls++

		name := call.ToolName

		messages = append(messages, activityMessage(ActivityRequest, call, nil, ""))

		tool, known := e.tools[name]

		// decode before announcing, so the event carries usable arguments
		arguments, decodeErr := decodeArguments(call)

		emit(Event{Kind: EventToolCallStart, Tool: name, Args: arguments, Text: call.Input})

		if !known {
			failure := fmt.Sprintf("no such tool: %s", name)

			emit(Event{Kind: EventToolCallError, Tool: name, Text: failure})

			messages = append(messages, activityMessage(ActivityResponse, call, nil, failure))

			continue
		}

		if decodeErr != nil {
			emit(Event{Kind: EventToolCallError, Tool: name, Text: decodeErr.Error()})

			messages = append(messages, activityMessage(ActivityResponse, call, nil, decodeErr.Error()))

			continue
		}

		// the turn's reasoning, text and this request go to the caller before the
		// handler runs: a shell call can outlast the run, and a run killed inside
		// one must still leave what the model thought and asked for
		e.handOver(messages)

		response, err := tool.Run(ctx, fantasy.ToolCall{ID: call.ToolCallID, Name: name, Input: call.Input})

		// a tool that ran and reported a problem is answered the same way as
		// one that could not run: the model reads the failure and acts on it
		if err == nil && response.IsError {
			err = errors.New(response.Content)
		}

		if err != nil {
			emit(Event{Kind: EventToolCallError, Tool: name, Text: err.Error()})

			messages = append(messages, activityMessage(ActivityResponse, call, nil, err.Error()))

			continue
		}

		emit(Event{Kind: EventToolCallEnd, Tool: name, Result: response.Content})

		messages = append(messages, activityMessage(ActivityResponse, call, response.Content, ""))
	}

	return messages, nil
}

// handOver gives the conversation as it stands to the OnConversation hook.
func (e *Engine) handOver(messages []Message) {
	if e.options.OnConversation != nil {
		e.options.OnConversation(messages)
	}
}

// activityMessage renders one half of a tool-call pair.
func activityMessage(kind ActivityKind, call fantasy.ToolCallContent, result any, failure string) Message {
	activity := &Activity{
		Kind:      kind,
		ID:        call.ToolCallID,
		Name:      call.ToolName,
		Arguments: call.Input,
	}

	if kind == ActivityResponse {
		activity.Result = result
		activity.Failure = failure
	}

	// the text is what the model is shown; a request shows nothing, because the
	// call itself is carried in the wire format's tool_calls field
	return Message{
		Type:     TypeActivity,
		Text:     activity.ResultText(),
		Activity: activity,
	}
}

// checkCycle looks for repetition and nudges the model, or stops the run once
// nudging has failed enough times.
func (e *Engine) checkCycle(messages []Message, budget *Budget) ([]Message, *Result) {
	detected := thread.DescribeThreadCycle(toThreadMessages(messages), thread.CycleOptions{})

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

	return append(messages, Message{Type: TypeUser, Text: cycleNotice(cycleDetail(detected))}), nil
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

// narrowInputBudget lowers the token budget the thread is trimmed to, after a
// provider rejected a request as too long. It reports whether the budget went
// down - if not there is nothing left to try, and the rejection is a real
// failure.
//
// The provider's stated window beats the configured one. A rejection is
// precisely the case where the configured window was wrong - a serving endpoint
// with a smaller ceiling than the operator stated - so believing the error is
// what makes the retry fit instead of guessing again. A rejection that states no window, or one
// no lower than the budget already in force, still has to shrink something or
// the retry would send the identical request: the budget steps down by a quarter
// instead, until it reaches the floor the instructions and tool schemas need.
//
// Only the budget changes. The conversation itself is untouched; the thread
// builder drops the oldest messages to fit it on the next request.
func (e *Engine) narrowInputBudget(limit ContextLimit, emit func(Event)) bool {
	if limit.SuggestedLimit > 0 && limit.SuggestedLimit < e.inputBudget {
		e.inputBudget = limit.SuggestedLimit

		emit(Event{Kind: EventRetry, Text: fmt.Sprintf(
			"provider reported a %d token window; retrying under %d",
			limit.MaxTokens, limit.SuggestedLimit)})

		return true
	}

	narrowed := e.inputBudget * 3 / 4

	if narrowed < MinInputTokens {
		return false
	}

	e.inputBudget = narrowed

	emit(Event{Kind: EventRetry, Text: fmt.Sprintf(
		"provider rejected the request as too long; retrying under %d tokens", narrowed)})

	return true
}

// buildRequest assembles the provider request, trimming the conversation to fit.
func (e *Engine) buildRequest(messages []Message, tools []fantasy.Tool) (fantasy.Call, error) {
	instructions := e.instructions()

	// reserve room for the system prompt and the tool schemas, both of which are
	// sent on every request and neither of which the thread builder sees
	reserved := estimateTokens(instructions) + e.toolSchemaTokens(tools)

	budget := e.inputBudget - reserved

	if budget < MinInputTokens/2 {
		budget = MinInputTokens / 2
	}

	built, err := thread.BuildThread(thread.BuildOptions{
		Messages:  toThreadMessages(messages),
		MaxTokens: float64(budget),

		// keep the most recent exchange whatever it costs, so a large tool
		// result cannot starve the turn that has to interpret it
		MinMessages: 2,

		Estimate: func(message thread.Message) (thread.Usage, error) {
			// A tool call carries almost all its cost outside the text - the name,
			// arguments and result live in the meta - so a request half (no text at
			// all) would be priced as empty and a write of a whole file would look
			// free to the trimmer, letting a thread that "fits" get rejected. Count
			// text plus the serialised meta, exactly as the source estimator does.
			text := message.Text()

			if meta, ok := message.Meta(); ok {
				if encoded, err := json.Marshal(meta); err == nil {
					text += string(encoded)
				}
			}

			return thread.Usage{
				Tokens: float64(estimateMessageTokens(text)),
			}, nil
		},
	})
	if err != nil {
		return fantasy.Call{}, err
	}

	chat := fantasy.Prompt{fantasy.NewSystemMessage(instructions)}

	chat = append(chat, toPrompt(fromThreadMessages(built.Messages))...)

	// The thread builder keeps the largest suffix that fits, so the first
	// message trimmed is the oldest - which is the run's opening user message.
	// A conversation with no user turn at all is invalid to strict providers:
	// they reject the whole request, deterministically, from that iteration on
	// (bisected live against one that answered only an opaque 400). The
	// objective itself is safe in the instructions; what must be restored is a
	// user turn's existence.
	hasUser := false

	for _, message := range chat[1:] {
		if message.Role == fantasy.MessageRoleUser {
			hasUser = true

			break
		}
	}

	if !hasUser {
		rest := append(fantasy.Prompt{fantasy.NewUserMessage(trimmedKickoff)}, chat[1:]...)
		chat = append(chat[:1], rest...)
	}

	call := fantasy.Call{Prompt: chat, Tools: tools}

	if e.options.MaxTokens != nil {
		limit := int64(*e.options.MaxTokens)

		call.MaxOutputTokens = &limit
	}

	return call, nil
}

// trimmedKickoff stands in for the opening user message once trimming has
// dropped it. The objective lives in the instructions, so this only has to
// exist and point there.
const trimmedKickoff = "Continue working on your task as stated in the instructions."

// instructions renders the system prompt.
func (e *Engine) instructions() string {
	var builder strings.Builder

	builder.WriteString(e.options.Instructions)

	if e.settleMode() {
		fmt.Fprintf(&builder,
			"\n\nWhen the objective is met, call %s. If it cannot be met, call %s. "+
				"The run is not finished until you call one of them.",
			SuccessTool, FailureTool,
		)
	}

	return builder.String()
}

// toolDefinitions renders the tool schemas, adding the terminal tools in settle
// mode.
func (e *Engine) toolDefinitions() []fantasy.Tool {
	var offered []fantasy.AgentTool

	offered = append(offered, e.options.Tools...)

	if e.settleMode() {
		offered = append(offered, terminalTools()...)
	}

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

func (e *Engine) finish(messages []Message, budget Budget, reason StopReason, detail string, err error) Result {
	return Result{
		Reason:   reason,
		Message:  detail,
		Messages: messages,
		Budget:   budget,
		Err:      err,
	}
}
