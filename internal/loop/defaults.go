package loop

// The defaults of the context window rules, which move with the window fitting.
const (
	// The share of the configured window a rejection can narrow
	// the effective window down to, as a divisor. A provider that keeps saying
	// "too long" is wrong about its own ceiling only so far.
	NarrowFloor = 4

	// DefaultContextSoft is the share of the context window, in percent, at which
	// the oldest message starts being forgotten on every request.
	DefaultContextSoft = 50

	// DefaultPlanNudgeEvery is how many iterations pass between reminders that
	// the plan tool exists and should be kept current.
	DefaultPlanNudgeEvery = 5

	// DefaultPlanMinTurns is how few turns may be left in the window, after
	// forgetting, before the plan is posted again. A window that holds fewer than
	// this has probably lost the model's last word on it.
	DefaultPlanMinTurns = 5

	// DefaultContextHard is the share of the window a request is never allowed to
	// reach. Past it, as many of the oldest messages are forgotten as it takes.
	DefaultContextHard = 90
)
