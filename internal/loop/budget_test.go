package loop_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/conversation"
	"github.com/openzot/openzot/internal/failure"
	"github.com/openzot/openzot/internal/loop"
	"github.com/openzot/openzot/internal/testutils"
)

// Budget semantics. The budgets look interchangeable and are not. Iterations count every trip round the loop and
// continuations only the times the model was cut off mid-answer. Conflating them kills a run making steady progress for
// "running out of output space", or lets a model that is always truncated run forever.

// A time cap ends a run at the iteration boundary. Unbounded by default, but a
// run pinned to a deadline must stop with StopTime rather than running to the
// iteration fallback.
func TestATimeBudgetStopsTheRun(t *testing.T) {
	calls := 0

	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		// a stub that calls a tool forever
		Client:        testutils.ScriptedClient(t, []string{testutils.Tool("call_1", litEcho, "{}")}),
		Tools:         echoTool(&calls),
		MaxDuration:   time.Millisecond,
		MaxIterations: 100000, // high, so time is what stops it, not iterations
		MaxCycles:     100000, // high, so cycle detection is not what stops it
	})

	assert.Equal(t, loop.StopTime, result.Reason)

	// it did some work before the deadline, and nowhere near the iteration cap
	assert.Less(t, result.Budget.Iterations, 100000, "want the time cap to bite first")
}

// With no time cap, a run is never stopped for time - the default is unbounded.
func TestTimeIsUnboundedByDefault(t *testing.T) {
	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client:        testutils.ScriptedClient(t, []string{testutils.Settle("done")}),
		MaxIterations: 5,
	})

	assert.NotEqual(t, loop.StopTime, result.Reason, "a run with no time cap must never stop for time")
}

// A tool round is progress. It costs an iteration and a call, and nothing else.
func TestToolRoundsDoNotSpendTheContinuationBudget(t *testing.T) {
	calls := 0

	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client: testutils.ScriptedClient(t,
			[]string{testutils.Tool("call_1", litEcho, "{}")},
			[]string{testutils.Tool("call_2", litEcho, "{}")},
			[]string{testutils.Settle("done")},
		),
		Tools:            echoTool(&calls),
		MaxIterations:    10,
		MaxContinuations: 1,
	})

	assert.Equal(t, 0, result.Budget.Recoveries, "tool rounds spent %d continuations, want 0", result.Budget.Recoveries)

	assert.Equal(t, 2, result.Budget.Calls)

	assert.Equal(t, loop.StopSettled, result.Reason, "want the run to finish normally")
}

// Being cut off mid-answer is not progress, and it is the only thing the
// continuation budget is there to bound.
func TestTruncationSpendsTheContinuationBudget(t *testing.T) {
	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client: testutils.ScriptedClient(t,
			[]string{testutils.Text("half an ans"), testutils.Truncated()},
			[]string{testutils.Settle("wer")},
		),
		MaxIterations: 10,
	})

	assert.Equal(t, 1, result.Budget.Recoveries)
}

// The two budgets are independent. A run can exhaust one while the other is
// barely touched, and neither may end the run on the other's behalf.
func TestTheTwoBudgetsAreIndependent(t *testing.T) {
	calls := 0

	// tool calls forever, with a continuation budget of one
	result := run(t, &loop.Options{
		ContextWindow:    testWindow,
		Client:           testutils.ScriptedClient(t, []string{testutils.Tool("call_1", litEcho, "{}")}),
		Tools:            echoTool(&calls),
		MaxIterations:    4,
		MaxContinuations: 1,
		MaxCycles:        1000,
	})

	assert.Equal(t, loop.StopIterations, result.Reason, "want the iteration budget to be what stops it")

	assert.Equal(t, 0, result.Budget.Recoveries, "want the continuation budget untouched")

	// and the other way round. Truncated forever, with plenty of iterations
	result = run(t, &loop.Options{
		ContextWindow:    testWindow,
		Client:           testutils.ScriptedClient(t, []string{testutils.Text("more"), testutils.Truncated()}),
		MaxIterations:    50,
		MaxContinuations: 3,
	})

	assert.Equal(t, loop.StopContinuations, result.Reason, "want the continuation budget to be what stops it")

	assert.Less(t, result.Budget.Iterations, 50, "want the continuation budget to bite first")
}

// Everything that goes round the loop costs an iteration - tool rounds,
// truncation retries, empty turns and settle nudges alike. It is the fallback
// that bounds a run no matter which way the model misbehaves.
func TestEveryKindOfRoundCostsAnIteration(t *testing.T) {
	tests := []struct {
		name  string
		turns [][]string
		tools []fantasy.AgentTool
	}{
		{
			name:  "tool rounds",
			turns: [][]string{{testutils.Tool("call_1", litEcho, "{}")}},
			tools: echoTool(new(int)),
		},
		{
			name:  "truncation retries",
			turns: [][]string{{testutils.Text("more"), testutils.Truncated()}},
		},
		{
			name:  "empty turns",
			turns: [][]string{{testutils.Stop()}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := run(t, &loop.Options{
				ContextWindow:    testWindow,
				Client:           testutils.ScriptedClient(t, test.turns...),
				Tools:            test.tools,
				MaxIterations:    3,
				MaxCalls:         100,
				MaxContinuations: 100,
				MaxEmpties:       100,
			})

			assert.Equal(t, 3, result.Budget.Iterations)

			assert.Equal(t, loop.StopIterations, result.Reason)
		})
	}
}

// One iteration is single-step mode. One model call, then stop. It is what a
// caller uses to drive the loop themselves.
func TestASingleIterationIsOneModelCall(t *testing.T) {
	calls := 0

	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client:        testutils.ScriptedClient(t, []string{testutils.Tool("call_1", litEcho, "{}")}),
		Tools:         echoTool(&calls),
		MaxIterations: 1,
		MaxCycles:     1000,
	})

	assert.Equal(t, 1, result.Budget.Iterations)

	assert.Equal(t, 1, calls, "the tool ran %d times, want once", calls)

	assert.Equal(t, loop.StopIterations, result.Reason)
}

// A non-positive budget means "unset", not "zero". The iteration count and the no-progress guards (cycles, empties) are hard
// fallbacks and must never be left unbounded, while unbounded is the intended default for the call and time budgets.
func TestBudgetDefaults(t *testing.T) {
	for _, value := range []int{0, -1, -1000} {
		engine, err := loop.New(&loop.Options{
			ContextWindow: testWindow,
			Client:        testutils.ScriptedClient(t, []string{testutils.Stop()}),
			MaxCalls:      value,
			MaxIterations: value,
			MaxCycles:     value,
			MaxEmpties:    value,
		})
		require.NoError(t, err)

		// fallbacks fall back to their finite defaults
		assert.Equal(t, loop.DefaultMaxIterations, engine.MaxIterations, "want the default backstop")

		assert.Positive(t, engine.MaxCycles, "a budget of %d left a guard unbounded: cycles=%d empties=%d", value, engine.MaxCycles, engine.MaxEmpties)
		assert.Positive(t, engine.MaxEmpties, "a budget of %d left a guard unbounded: cycles=%d empties=%d", value, engine.MaxCycles, engine.MaxEmpties)

		// calls stays unbounded - a non-positive value is "no cap", not a default
		assert.Equal(t, 0, engine.MaxCalls, "a budget of %d gave maxCalls=%d, want 0 (unbounded)", value, engine.MaxCalls)
	}
}

// Deep agentic loops must be stack-safe. The TypeScript engine recursed per
// round and had to be rewritten when a long run overflowed. This documents that
// agent's loop is iterative and cannot.
func TestADeepRunDoesNotGrowTheStack(t *testing.T) {
	calls := 0

	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client:        testutils.ScriptedClient(t, []string{testutils.Tool("call_1", litEcho, "{}")}),
		Tools:         echoTool(&calls),
		MaxIterations: 500,
		MaxCalls:      500,
		MaxCycles:     1000,
	})

	assert.Equal(t, 500, result.Budget.Iterations)

	assert.GreaterOrEqual(t, calls, 400, "the tool ran %d times over 500 rounds", calls)
}

// mentionsAFailure reports whether any tool result carries an error.
func mentionsAFailure(messages []conversation.Message) bool {
	for _, message := range messages {
		if message.Activity != nil && message.Activity.Failure != "" {
			return true
		}
	}

	return false
}

// Arguments that cannot be read even after repair are the model's mistake to
// correct. Invoking the handler with a guess would be worse than not invoking it
// at all.
func TestMalformedArgumentsReachTheModelNotTheHandler(t *testing.T) {
	invoked := 0

	tools := []fantasy.AgentTool{namedTool(litEcho, func(context.Context) (any, error) {
		invoked++

		return "ok", nil
	})}

	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client: testutils.ScriptedClient(t,
			[]string{testutils.Tool("call_1", litEcho, `not json at all`)},
			[]string{testutils.Settle("let me try that again")},
		),
		Tools:         tools,
		MaxIterations: 5,
	})

	assert.Equal(t, 0, invoked, "the handler ran %d times on undecodable arguments", invoked)

	assert.True(t, mentionsAFailure(result.Messages), "the decode failure must be fed back so the model can correct it")

	assert.Equal(t, loop.StopSettled, result.Reason)
}

// fantasy repairs what it can before a call is run - a missing closing brace or
// quote, a trailing comma - so a model's small slips cost no turn. The tool sees
// the repaired arguments.
func TestSlightlyMalformedArgumentsAreRepairedAndRun(t *testing.T) {
	invoked := 0

	tools := []fantasy.AgentTool{namedTool(litEcho, func(context.Context) (any, error) {
		invoked++

		return "ok", nil
	})}

	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client: testutils.ScriptedClient(t,
			[]string{testutils.Tool("call_1", litEcho, `{"value": "abc`)},
			[]string{testutils.Settle("done")},
		),
		Tools:         tools,
		MaxIterations: 5,
	})

	assert.Equal(t, 1, invoked, "want the repaired call to run once")

	assert.False(t, mentionsAFailure(result.Messages), "a call that could be repaired must not be reported as a failure")

	assert.Equal(t, loop.StopSettled, result.Reason)
}

// containsText reports whether any message holds the given text.
func containsText(messages []conversation.Message, want string) bool {
	for _, message := range messages {
		if strings.Contains(message.Text, want) {
			return true
		}
	}

	return false
}

// A tool that fails is information, not an outage. The run continues with the
// error in hand.
func TestAFailingToolIsReportedAndTheRunContinues(t *testing.T) {
	tools := []fantasy.AgentTool{namedTool(litEcho, func(context.Context) (any, error) {
		return nil, errors.New("permission denied")
	})}

	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client: testutils.ScriptedClient(t,
			[]string{testutils.Tool("call_1", litEcho, "{}")},
			[]string{testutils.Settle("understood")},
		),
		Tools:         tools,
		MaxIterations: 5,
	})

	assert.Equal(t, loop.StopSettled, result.Reason, "want the run to survive a failing tool")

	assert.True(t, containsText(result.Messages, "permission denied"), "the failure must be visible to the model")
}

// countActivities counts each half of the tool-call pairs.
func countActivities(messages []conversation.Message) (requests, responses int) {
	for _, message := range messages {
		if message.Activity == nil {
			continue
		}

		switch message.Activity.Kind {
		case conversation.ActivityRequest:
			requests++
		case conversation.ActivityResponse:
			responses++
		default:
			// a trigger is neither half
		}
	}

	return requests, responses
}

// A handler that returns nothing still has to produce a result message, or the
// call is left unanswered and the next request is invalid.
func TestAHandlerReturningNothingStillAnswersTheCall(t *testing.T) {
	tools := []fantasy.AgentTool{namedTool(litEcho, func(context.Context) (any, error) {
		var nothing any

		return nothing, nil
	})}

	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client: testutils.ScriptedClient(t,
			[]string{testutils.Tool("call_1", litEcho, "{}")},
			[]string{testutils.Settle("done")},
		),
		Tools:         tools,
		MaxIterations: 5,
	})

	requests, responses := countActivities(result.Messages)

	assert.Equal(t, responses, requests, "want them paired")
	assert.NotEqual(t, 0, requests, "want them paired")
}

// A finish reason agent has no special handling for - content_filter is the one
// providers actually send - must not derail the run.
func TestAnUnrecognisedFinishReasonIsNotFatal(t *testing.T) {
	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client: testutils.ScriptedClient(t,
			[]string{
				testutils.Text("I cannot help with that"),
				`{"choices":[{"delta":{},"finish_reason":"content_filter"}]}`,
			},
			[]string{testutils.Settle("stopped")},
		),
		MaxIterations: 5,
	})

	// the filtered turn is answered like any turn that stops without acting. A
	// nudge to settle, and the run carries on
	assert.Equal(t, loop.StopSettled, result.Reason, "want the run to carry on and settle after one")
	assert.Equal(t, 1, result.Budget.Settles, "want the run to carry on and settle after one")

	require.NoError(t, result.Err)
}

// A turn that claims tool calls and carries none is a provider bug. It has to
// degrade to an empty turn rather than panic on the missing payload.
func TestAToolCallFinishWithNoCallsIsNotFatal(t *testing.T) {
	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client: testutils.ScriptedClient(t, []string{
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		}),
		MaxIterations: 3,
		MaxEmpties:    2,
	})

	require.NoError(t, result.Err)

	assert.Contains(t, []loop.StopReason{loop.StopEmpty, loop.StopIterations}, result.Reason, "want the turn treated as empty")
}

// A retriable provider failure has to be waited out, not hammered. Retrying instantly spends the whole continuation budget
// inside one outage in a few milliseconds, so a run dies to a blip that a short pause would have outlived.
func TestRetriableFailuresAreSpacedOut(t *testing.T) {
	client := testutils.Script(t, testutils.Reject(http.StatusInternalServerError, "")).Client(t)

	started := time.Now()

	result := run(t, &loop.Options{
		ContextWindow:    testWindow,
		Client:           client,
		Messages:         []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxContinuations: 3,
		MaxIterations:    50,
		RetryBackoff:     20 * time.Millisecond,
	})

	elapsed := time.Since(started)

	require.Equal(t, loop.StopError, result.Reason, "want the run to end on the provider failure")

	require.Equal(t, 3, result.Budget.Recoveries)

	// 20ms, then 40ms, then 80ms. The doubling means three retries cannot fit
	// into anything close to the zero delay they used to take.
	want := 100 * time.Millisecond
	assert.GreaterOrEqual(t, elapsed, want, "three retries took %s, want at least %s of backoff between them", elapsed, want)

	// A failed model call is not an agentic round. Continuations bound recovery, so charging the
	// iteration budget too would make an outage cost the run twice.
	assert.Equal(t, 0, result.Budget.Iterations, "want 0 - no round ever completed")
}

// Canceling a run must cut a backoff short rather than making the caller wait
// out a pause that no longer has a retry at the end of it.
func TestBackoffEndsWhenTheRunIsCancelled(t *testing.T) {
	client := testutils.Script(t, testutils.Reject(http.StatusInternalServerError, "")).Client(t)

	engine, err := loop.New(&loop.Options{
		ContextWindow:    testWindow,
		Client:           client,
		Messages:         []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxContinuations: 5,
		RetryBackoff:     time.Hour,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	started := time.Now()
	result := engine.Run(ctx, nil)

	elapsed := time.Since(started)
	require.LessOrEqual(t, elapsed, 30*time.Second, "cancellation took %s to end an hour-long backoff", elapsed)

	assert.Equal(t, loop.StopAborted, result.Reason, "want the cancellation to end the run")

	// The abort landed during a backoff wait, but the provider failure before it travels with it as
	// evidence. A bare "context canceled" would discard the exchange the operator quit to read.
	require.Error(t, result.Err, "want the last provider failure preserved")
	assert.True(t, failure.IsProviderError(result.Err), "want the last provider failure preserved")
}

// The default backoff must be a real pause. A zero default would silently
// restore the tight retry loop. Asserted on the constructed engine because
// reaching it behaviourally costs a second of wall clock per retry.
func TestRetryBackoffDefaultsToARealPause(t *testing.T) {
	engine, err := loop.New(&loop.Options{ContextWindow: testWindow, Client: testutils.ScriptedClient(t, []string{testutils.Stop()})})
	require.NoError(t, err)

	assert.Positive(t, engine.RetryBackoff, "want a positive pause")

	// and a caller can still opt out, which is what keeps these tests fast
	engine, err = loop.New(&loop.Options{ContextWindow: testWindow, Client: testutils.ScriptedClient(t, []string{testutils.Stop()}), RetryBackoff: -1})
	require.NoError(t, err)

	got := loop.BackoffFor(engine.RetryBackoff, 1)
	assert.EqualValues(t, 0, got, "opted-out backoff = %s, want none", got)
}

// The pause doubles per consecutive retry so a persistent outage is not retried
// at the same rate as a one-off blip, and is capped so a long continuation
// budget cannot leave a run asleep for hours.
func TestBackoffDoublesAndIsCapped(t *testing.T) {
	base := time.Second

	got := loop.BackoffFor(base, 1)
	assert.Equal(t, base, got, "first retry waits %s, want %s", got, base)

	got = loop.BackoffFor(base, 2)
	assert.Equal(t, 2*base, got, "second retry waits %s, want %s", got, 2*base)

	got = loop.BackoffFor(base, 3)
	assert.Equal(t, 4*base, got, "third retry waits %s, want %s", got, 4*base)

	got = loop.BackoffFor(base, 40)
	assert.Equal(t, loop.MaxRetryBackoff, got, "a long outage waits %s, want the cap %s", got, loop.MaxRetryBackoff)

	// the cap binds the base too. A caller-configured backoff above it must not
	// make the first retry the longest wait of the run
	got = loop.BackoffFor(2*loop.MaxRetryBackoff, 1)
	assert.Equal(t, loop.MaxRetryBackoff, got, "a base above the cap waits %s on the first retry, want the cap %s", got, loop.MaxRetryBackoff)
}

// A rate limit must not kill a run. 429 is excluded from IsRetriable because it needs the provider's own schedule, but with
// nothing waiting on it the loop fell through to StopError, so one throttle response ended an overnight run.
func TestARateLimitIsWaitedOutRatherThanFatal(t *testing.T) {
	client := testutils.Script(t,
		testutils.Reject(http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`).WithHeader("Retry-After", "1"),
		testutils.Frames(testutils.Tool("c1", loop.SuccessTool, `{"summary":"done anyway"}`)),
	).Client(t)

	started := time.Now()

	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client:        client,
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxSettles:    5,
	})

	elapsed := time.Since(started)

	require.Equal(t, loop.StopSettled, result.Reason, "want the run to survive the rate limit")

	assert.Equal(t, 1, result.Budget.Recoveries, "want the rate limit to cost exactly one")

	// the provider asked for a second. Honoring that is the whole point, so a
	// retry that came back sooner means the advice was ignored
	assert.GreaterOrEqual(t, elapsed, time.Second, "want the advised second to be waited out")
}

// A provider that advises an absurd Retry-After must not park an unattended run
// for hours. The advice is honored up to a cap, and no further.
func TestAnAbsurdRetryAfterIsCapped(t *testing.T) {
	assert.Equal(t, loop.MaxRateLimitWait, loop.RateLimitWait(48*time.Hour, true, time.Second))

	// advice inside the cap is followed exactly, rather than rounded to our own
	// backoff schedule
	assert.Equal(t, 90*time.Second, loop.RateLimitWait(90*time.Second, true, time.Second))

	// and with no advice at all the ordinary backoff applies
	assert.Equal(t, 4*time.Second, loop.RateLimitWait(0, false, 4*time.Second), "want the fallback backoff")
}

// The backoff is a floor under the provider's advice, not only a fallback for its absence. "Retry-After: 0" is advice to
// retry now, and a provider that keeps sending it would otherwise be hammered with the tight loop the backoff prevents.
func TestAZeroRetryAfterIsFlooredByTheBackoff(t *testing.T) {
	assert.Equal(t, 4*time.Second, loop.RateLimitWait(0, true, 4*time.Second), "want the 4s backoff floor under \"retry now\"")

	// advice above the floor still wins. The provider knows its own window
	assert.Equal(t, 90*time.Second, loop.RateLimitWait(90*time.Second, true, 4*time.Second), "want the advised 90s over the smaller backoff")
}

// And end to end. Repeated 429s advising "retry now" must still space their
// retries out on the backoff schedule rather than burning the continuation
// budget in milliseconds.
func TestRepeated429WithZeroRetryAfterStillBacksOff(t *testing.T) {
	client := testutils.Script(t,
		testutils.Reject(http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`).WithHeader("Retry-After", "0"),
	).Client(t)

	started := time.Now()

	result := run(t, &loop.Options{
		ContextWindow:    testWindow,
		Client:           client,
		Messages:         []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxContinuations: 3,
		MaxIterations:    50,
		RetryBackoff:     20 * time.Millisecond,
	})

	elapsed := time.Since(started)

	require.Equal(t, loop.StopError, result.Reason, "want the run to end once the budget is spent")

	require.Equal(t, 3, result.Budget.Recoveries)

	// 20ms, then 40ms, then 80ms. The advised zero must not undercut the floor
	want := 100 * time.Millisecond
	assert.GreaterOrEqual(t, elapsed, want, "three rate-limited retries took %s, want at least %s of backoff between them", elapsed, want)
}

// The backoff paces consecutive failures. Once a turn succeeds the outage is over, and the next blip, hours later, must
// start again from the base delay rather than wherever the last outage left the schedule.
func TestBackoffRestartsAfterASuccessfulTurn(t *testing.T) {
	// a two-deep outage waits base, then 2x base. A successful tool round ends it, and a fresh, unrelated
	// blip must then wait base again, not 4x base
	client := testutils.Script(t,
		testutils.Reject(http.StatusInternalServerError, ""), testutils.Reject(http.StatusInternalServerError, ""),
		testutils.Frames(testutils.Tool("c1", litEcho, `{}`)),
		testutils.Reject(http.StatusInternalServerError, ""),
		testutils.Frames(testutils.Tool("c2", loop.SuccessTool, `{"summary":"done"}`)),
	).Client(t)

	calls := 0

	base := 300 * time.Millisecond

	started := time.Now()

	result := run(t, &loop.Options{
		ContextWindow:    testWindow,
		Client:           client,
		Tools:            echoTool(&calls),
		Messages:         []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxContinuations: 10,
		MaxIterations:    20,
		MaxSettles:       5,
		RetryBackoff:     base,
	})

	elapsed := time.Since(started)

	require.Equal(t, loop.StopSettled, result.Reason)

	require.Equal(t, 3, result.Budget.Recoveries)

	// The wait is base + 2x base + base = 4x base when the counter resets on success. A counter that
	// kept escalating would wait 7x base. The bound sits between, with slack for a loaded machine.
	floor := 4 * base
	require.GreaterOrEqual(t, elapsed, floor, "the retries took %s, want at least %s of backoff", elapsed, floor)

	ceiling := 6 * base
	assert.LessOrEqual(t, elapsed, ceiling, "the retries took %s, want under %s - the backoff must restart from the base after a successful turn", elapsed, ceiling)
}

// The consecutive-failure counter is the backoff's own, not the continuation budget, which truncation recoveries and
// context-limit retries also spend. Keying the backoff off it started an unrelated first blip at 8x base.
func TestOtherContinuationsDoNotEscalateTheBackoff(t *testing.T) {
	// three truncated answers each spend a continuation and none is a failure. The run's first retriable
	// failure must then wait base, not 8x base
	truncatedTurn := testutils.Frames(testutils.Text("more to say"), testutils.Truncated())

	client := testutils.Script(t,
		truncatedTurn, truncatedTurn, truncatedTurn,
		testutils.Reject(http.StatusInternalServerError, ""),
		testutils.Frames(testutils.Settle("done")),
	).Client(t)

	base := 300 * time.Millisecond

	started := time.Now()

	result := run(t, &loop.Options{
		ContextWindow:    testWindow,
		Client:           client,
		Messages:         []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxContinuations: 10,
		MaxIterations:    20,
		RetryBackoff:     base,
	})

	elapsed := time.Since(started)

	require.Equal(t, loop.StopSettled, result.Reason)

	require.Equal(t, 4, result.Budget.Recoveries, "want 3 truncations plus 1 retry")

	// one failure, one wait of base. Keyed off the shared budget it would have
	// been 8x base. The bound leaves generous slack for a loaded machine.
	floor := base
	require.GreaterOrEqual(t, elapsed, floor, "the retry took %s, want at least the %s base backoff", elapsed, floor)

	ceiling := 4 * base
	assert.LessOrEqual(t, elapsed, ceiling, "the retry took %s, want under %s - truncation recoveries must not escalate the failure backoff", elapsed, ceiling)
}

// A stalling provider that returns an empty turn renders as bare iteration dividers unless the nudge is surfaced. A run being
// nudged back to life looked exactly like a hang, as when a live provider held a stream for three silent minutes.
func TestAnEmptyTurnEmitsAVisibleNotice(t *testing.T) {
	engine, err := loop.New(&loop.Options{
		ContextWindow: testWindow,
		Client: testutils.ScriptedClient(t,
			[]string{testutils.Stop()},
			[]string{testutils.Settle("recovered")},
		),
		MaxIterations: 5,
		MaxEmpties:    3,
		RetryBackoff:  -1,
	})
	require.NoError(t, err)

	var notices []string

	result := engine.Run(t.Context(), func(event loop.Event) {
		if event.Kind == loop.EventNotice {
			notices = append(notices, event.Text)
		}
	})

	require.Equal(t, loop.StopSettled, result.Reason)

	var noticed bool

	for _, notice := range notices {
		if strings.Contains(notice, "empty turn") {
			noticed = true
		}
	}

	assert.True(t, noticed, "the empty-turn nudge must be visible; silence reads as a hang (notices: %v)", notices)
}

// The continuation bound asks whether this run can get going again, not how much has gone wrong since it started. Blips that
// were each recovered from prove nothing, and a run working for hours must not be ended by its twenty-first.
func TestRecoveredBlipsDoNotAddUp(t *testing.T) {
	// alternating. Blip, good turn, blip, good turn... six blips in all, well
	// past a continuation budget of two, none of them consecutive
	client := testutils.NewServer(t, func(request int, _ string) testutils.Turn {
		switch {
		case request%2 == 1 && request <= 11:
			return testutils.Reject(http.StatusInternalServerError, "")
		case request <= 11:
			return testutils.Frames(testutils.Tool(fmt.Sprintf("c%d", request), litEcho, `{}`))
		default:
			return testutils.Frames(testutils.Tool("done", loop.SuccessTool, `{"summary":"done"}`))
		}
	}).Client(t)

	calls := 0

	result := run(t, &loop.Options{
		ContextWindow:    testWindow,
		Client:           client,
		Tools:            echoTool(&calls),
		Messages:         []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxContinuations: 2,
		MaxIterations:    40,
		MaxSettles:       5,
		// the matching echo rounds are the point of the fixture, not a
		// repetition the run should be nudged out of
		MaxCycles:    10_000,
		RetryBackoff: -1,
	})

	require.Equal(t, loop.StopSettled, result.Reason, "want the run to finish - no two failures were consecutive")

	require.Equal(t, 6, result.Budget.Recoveries, "want all 6 blips recorded")

	assert.Equal(t, 0, result.Budget.Continuations, "want the count reset by the last good turn")
}

// The other side of the reset. Consecutive failures still end the run, and at
// the bound rather than somewhere past it.
func TestConsecutiveFailuresStillEndTheRun(t *testing.T) {
	client := testutils.Script(t, testutils.Reject(http.StatusInternalServerError, "")).Client(t)

	result := run(t, &loop.Options{
		ContextWindow:    testWindow,
		Client:           client,
		Messages:         []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxContinuations: 3,
		MaxIterations:    50,
		RetryBackoff:     -1,
	})

	require.Equal(t, loop.StopError, result.Reason, "want the run to end once the consecutive budget is spent")

	require.Equal(t, 3, result.Budget.Recoveries, "want exactly the bound")
}

// The shape MaxContinuations cannot see. A provider answering just often enough to keep resetting the consecutive count,
// while the run spends its life retrying, so the tally has to end it. Iterations are set far above the recovery bound to
// make it unambiguous which one fired.
func TestAChronicallyFailingProviderIsCalledBroken(t *testing.T) {
	// every odd request fails. Every even one is an empty-ish tool round that
	// resets the consecutive count. This never settles on its own.
	client := testutils.NewServer(t, func(request int, _ string) testutils.Turn {
		if request%2 == 1 {
			return testutils.Reject(http.StatusInternalServerError, "")
		}

		return testutils.Frames(testutils.Tool(fmt.Sprintf("c%d", request), litEcho, `{}`))
	}).Client(t)

	calls := 0

	const recoveries = 20

	result := run(t, &loop.Options{
		ContextWindow: testWindow,
		Client:        client,
		Tools:         echoTool(&calls),
		Messages:      []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		// generous, so a consecutive bound cannot be what fires
		MaxContinuations: 1_000,
		MaxRecoveries:    recoveries,
		// far above what the fallback allows, so it is the fallback that stops
		// this and not the round budget
		MaxIterations: 10_000,
		MaxCycles:     10_000,
		RetryBackoff:  -1,
	})

	require.Equal(t, loop.StopError, result.Reason, "want the recovery bound to end it")

	// the run ends naming the provider failure it kept papering over, not a
	// bare "budget spent" - the last error is what an operator needs
	require.Error(t, result.Err, "the run must carry the provider failure that ended it")

	require.Equal(t, recoveries, result.Budget.Recoveries)
}

// The recovery bound is absolute rather than a multiple of MaxContinuations. Tying them would let a caller who lowers the
// consecutive bound to fail fast silently lower the total too, and a long healthy run would die of scattered recovered blips.
func TestALowConsecutiveBoundDoesNotShrinkTheRecoveryBound(t *testing.T) {
	// blip, good turn, blip, good turn ... twenty blips, none consecutive
	client := testutils.NewServer(t, func(request int, _ string) testutils.Turn {
		switch {
		case request%2 == 1 && request <= 39:
			return testutils.Reject(http.StatusInternalServerError, "")
		case request <= 39:
			return testutils.Frames(testutils.Tool(fmt.Sprintf("c%d", request), litEcho, `{}`))
		default:
			return testutils.Frames(testutils.Tool("done", loop.SuccessTool, `{"summary":"done"}`))
		}
	}).Client(t)

	calls := 0

	// the tightest useful consecutive bound - fail fast on a stuck provider
	result := run(t, &loop.Options{
		ContextWindow:    testWindow,
		Client:           client,
		Tools:            echoTool(&calls),
		Messages:         []conversation.Message{{Type: conversation.TypeUser, Text: "go"}},
		MaxContinuations: 1,
		MaxIterations:    100,
		MaxCycles:        10_000,
		MaxSettles:       5,
		RetryBackoff:     -1,
	})

	require.Equal(t, loop.StopSettled, result.Reason, "want 20 recovered blips not to end a run that fails fast in a row")

	assert.Equal(t, 20, result.Budget.Recoveries)
}
