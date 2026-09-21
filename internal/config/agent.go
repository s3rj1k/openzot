package config

import (
	"fmt"
	"strings"
	"time"
)

// Agent holds the knobs that shape an autonomous run.
type Agent struct {
	// Model is the model name driving the agent.
	Model string `yaml:"model"`
	// MaxIterations caps how many plan/act/observe cycles the agent may run
	// before it is forced to stop.
	MaxIterations int `yaml:"max_iterations"`
	// How many times the agent is nudged to record an outcome (success or failure) before the run is
	// reported unsettled. Zero uses the built-in default.
	MaxSettles int `yaml:"max_settles"`
	// Caps total tool calls across a run, independent of iterations (one can request several).
	// Zero is unbounded, and max_iterations is the only finite default.
	MaxCalls int `yaml:"max_calls"`
	// MaxTime caps the wall-clock time of a run, as a duration string ("30m",
	// "2h", "90s"). Empty is unbounded.
	MaxTime string `yaml:"max_time"`
	// Caps the output tokens of one model response. Zero sends no cap, like max_calls and max_time,
	// so the model produces its full output.
	MaxTokens int `yaml:"max_tokens"`
	// Caps one tool result at this share of the context window, in percent, before it is truncated.
	// Zero uses the default (25). A share, not a size, so a small-window model gets a tighter bound.
	MaxToolOutputPercent int `yaml:"max_tool_output_percent"`
	// Caps CONSECUTIVE recovery attempts (truncated response, retriable provider error) with no
	// good turn between them. A whole turn resets the count. Zero uses the built-in default.
	MaxContinuations int `yaml:"max_continuations"`
	// Caps recovery attempts across a whole run, however spaced. max_continuations catches a provider
	// failing now, this catches one that answers just often enough to keep resetting it. Zero uses the default.
	MaxRecoveries int `yaml:"max_recoveries"`
	// How many times the loop nudges the model out of a detected repetition before giving up. Zero
	// uses the built-in default. The default encodes a real failure, so raise it with care.
	MaxCycles int `yaml:"max_cycles"`
	// MaxEmpties caps consecutive empty turns before the run bails. Zero uses the
	// built-in default.
	MaxEmpties int `yaml:"max_empties"`
	// Percent of the context window where the oldest message starts to be forgotten on every request.
	// Zero uses the built-in default (50).
	ContextSoft int `yaml:"context_soft"`
	// Percent of the window a request may never reach. Past it, as many of the oldest messages are
	// forgotten as it takes. Must be above context_soft. Zero uses the built-in default (90).
	ContextHard int `yaml:"context_hard"`
	// Iterations between reminders that the plan tool exists. Zero uses the built-in default (5).
	// A negative value turns the reminders off.
	PlanNudgeEvery int `yaml:"plan_nudge_every"`
	// How few turns may be left in the window after forgetting before the plan is posted again.
	// Zero uses the built-in default (5).
	PlanMinTurns int `yaml:"plan_min_turns"`
}

// MaxDuration parses Agent.MaxTime into a duration. An empty value is zero
// (unbounded). A malformed value is an error so a typo in the config is caught
// at load rather than silently ignored.
func (a *Agent) MaxDuration() (time.Duration, error) {
	value := strings.TrimSpace(a.MaxTime)
	if value == "" {
		return 0, nil
	}

	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration (use forms like \"30m\", \"2h\", \"90s\")", a.MaxTime)
	}

	if d < 0 {
		return 0, fmt.Errorf("%q is negative", a.MaxTime)
	}

	return d, nil
}

// The defaults of the two context thresholds live here because the rule between them is the config's. The
// engine has its own fallbacks for callers building options by hand, and a test holds the two to the same numbers.
const (
	defaultContextSoft = 50
	defaultContextHard = 90
)

// validateContext holds the thresholds to 1 <= soft < hard <= 99, percent of the
// window. Zero is the default.
func (a *Agent) validateContext() error {
	soft, hard := a.ContextSoft, a.ContextHard

	if soft == 0 {
		soft = defaultContextSoft
	}

	if hard == 0 {
		hard = defaultContextHard
	}

	if soft < 1 || hard > 99 || soft >= hard {
		return fmt.Errorf(
			"agent.context_soft/context_hard: soft=%d hard=%d: want 1 <= soft < hard <= 99 (percent of the window)", soft, hard)
	}

	return nil
}
