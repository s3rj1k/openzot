package loop_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/s3rj1k/agent/internal/loop"
	"github.com/s3rj1k/agent/internal/outcome"
)

func TestCycleDetailCoversEveryHeuristic(t *testing.T) {
	// each heuristic fails differently, so each needs its own explanation - a
	// generic nudge would not tell the model what to change
	for _, heuristic := range []string{
		"repeated_suffix",
		"repeated_activity_tail",
		"repeated_result_run",
		"repeated_message_text_run",
	} {
		assert.NotEmpty(t, loop.CycleDetail(heuristic), "heuristic %q has no explanation for the model", heuristic)
	}

	detail := loop.CycleDetail("something-new")
	assert.Empty(t, detail, "an unknown heuristic should fall back to the generic notice, got %q", detail)
}

// A caller scripting against agent tells success from everything else by the exit
// code. Only a run that settled is a success.
func TestExitCodeSeparatesSuccessFromEverythingElse(t *testing.T) {
	for reason, want := range map[outcome.StopReason]int{
		outcome.StopSettled:       0,
		outcome.StopFailed:        1,
		outcome.StopUnsettled:     1,
		outcome.StopIterations:    1,
		outcome.StopCalls:         1,
		outcome.StopTime:          1,
		outcome.StopContinuations: 1,
		outcome.StopCycle:         1,
		outcome.StopEmpty:         1,
		outcome.StopAborted:       1,
		outcome.StopError:         1,
	} {
		result := loop.Result{Reason: reason}

		assert.Equal(t, want, result.ExitCode(), "%s: exit code", reason)
	}
}
