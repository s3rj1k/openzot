// Package outcome is how a run is bounded, nudged and ended. The limits on what it may spend, the reasons it stops, the
// success and failure tools the model ends it with, and the notices that push it toward one of them.
package outcome

// Bounds on a single run. Every default here encodes a failure that happened.
const (
	// DefaultMaxIterations caps agentic rounds - one model call plus the tools
	// it requests. The loop is iterative rather than recursive, so any value is
	// safe. This is a behavioral bound, not a stack one.
	DefaultMaxIterations = 1000

	// DefaultMaxContinuations caps CONSECUTIVE recovery attempts - output
	// truncated, or a retriable provider error - with no good turn between
	// them. Distinct from iterations, which count normal progress.
	//
	// Consecutive, not lifetime, because a recovery that worked is not
	// evidence of anything. A run-lifetime count meant twenty transient blips
	// spread over four hours - each one recovered from, each one followed by
	// real work - left the run with no budget at all, and the twenty-first
	// blip ended it however healthy it was. The bound is meant to catch a run
	// that cannot get going again, so it asks that question and no other. It
	// zeroes the moment a turn comes back whole, exactly like the empty and
	// cycle counters.
	DefaultMaxContinuations = 20

	// DefaultMaxRecoveries caps recovery attempts across a whole run, however
	// they are spaced. The point at which a provider stops being given the
	// benefit of the doubt.
	//
	// MaxContinuations catches a provider refusing right now - twenty in a row
	// and the run is stuck. It cannot catch the other shape. One that answers
	// often enough to keep resetting the consecutive count, while the run
	// spends most of its life retrying rather than working. Every good turn
	// says the upstream is fine and the tally says it is not, and without this
	// the tally is never consulted.
	//
	// So the number has to be one a run can actually reach. A healthy run
	// spends recoveries in tens - a truncated answer continued, the occasional
	// 500, a context-limit retry - across an iteration budget that
	// defaults to a thousand. Two hundred is a fifth of that budget spent on
	// recovery instead of progress, which no working provider does. Set high
	// enough not to punish a long, output-heavy run that
	// continues a lot. Low enough that a chronically failing endpoint is
	// called what it is instead of nursed for hours.
	//
	// By design absolute rather than a multiple of MaxContinuations. The two
	// answer unrelated questions, and tying them together means lowering the
	// consecutive bound - the obvious thing to want, to fail fast on a stuck
	// provider - silently lowers this one into a range a long healthy run
	// reaches, which is the accumulating budget the consecutive count exists to
	// get rid of.
	DefaultMaxRecoveries = 200

	// DefaultMaxCycles is how many times the loop will nudge a model out of a
	// detected repetition before giving up on it.
	DefaultMaxCycles = 2

	// DefaultMaxEmpties caps turns that stop with neither text nor a tool call.
	// Far tighter than the continuation budget because retrying an empty turn
	// rarely recovers.
	DefaultMaxEmpties = 3

	// DefaultMaxSettles caps settle nudges. A run is finished only when the
	// model calls a terminal tool - never because its prose sounded final.
	DefaultMaxSettles = 20
)

// Terminal tool names. The model ends a run by calling one of these, which is
// unambiguous in a way prose never is.
const (
	SuccessTool = "success"
	FailureTool = "failure"
)

// StopReason explains why a run ended.
type StopReason string

const (
	// StopSettled - the model called the success tool. The only clean ending.
	StopSettled StopReason = "settled"

	// StopFailed - the model called the failure tool. It reached a conclusion,
	// and the conclusion is that the task cannot be done. A settled ending, but
	// not a successful one, so it must never be reported as StopSettled is.
	StopFailed StopReason = "failed"

	// StopIterations - the round budget ran out.
	StopIterations StopReason = "iterations"

	// StopCalls - the tool-call budget ran out.
	StopCalls StopReason = "calls"

	// StopContinuations - too many recovery attempts, either in a row or
	// across the run.
	StopContinuations StopReason = "continuations"

	// StopCycle - the model kept repeating itself after being nudged.
	StopCycle StopReason = "cycle"

	// StopTime - the wall-clock time budget ran out.
	StopTime StopReason = "time"

	// StopEmpty - too many turns produced nothing at all.
	StopEmpty StopReason = "empty"

	// StopUnsettled - the run exhausted its settle nudges without a terminal call.
	StopUnsettled StopReason = "unsettled"

	// StopAborted - the caller canceled.
	StopAborted StopReason = "aborted"

	// StopError - an unrecoverable failure.
	StopError StopReason = "error"
)

// Budget tracks what a run has spent.
type Budget struct {
	Iterations int
	Calls      int

	// Continuations counts CONSECUTIVE recovery attempts and a whole turn zeroes it. Recoveries is the
	// run total, which only rises. It is what the run reports spending and what DefaultMaxRecoveries bounds.
	Continuations int
	Recoveries    int

	Cycles  int
	Empties int
	Settles int

	// The provider-reported prompt and completion tokens across the run - actual billed usage, not the
	// local estimate. Each call bills its whole prompt, so they are summed per turn.
	InputTokens  int
	OutputTokens int
}

// spendContinuation records one recovery attempt against both counts. The
// consecutive run of them, and the total across the run.
func (b *Budget) SpendContinuation() {
	b.Continuations++
	b.Recoveries++
}
