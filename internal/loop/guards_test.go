package loop_test

import (
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/loop"
)

// Every notice must be recognizable as an injected instruction rather than the
// model's own words - the cycle detector skips them by that prefix, so a notice
// without it would break loop detection.
func TestNoticesCarryThePrefix(t *testing.T) {
	notices := map[string]string{
		"cycle":      loop.CycleNotice("you keep calling the same tool"),
		"settle":     loop.SettleNotice(),
		"truncation": loop.TruncationNotice(),
	}

	for name, notice := range notices {
		assert.True(t, strings.HasPrefix(notice, loop.NoticePrefix), "%s notice does not carry the prefix", name)

		assert.NotEmpty(t, strings.TrimSpace(strings.TrimPrefix(notice, loop.NoticePrefix)), "%s notice has no content", name)
	}
}

// A nudge that only says "you seem stuck" produces another lap. Naming the
// behavior is what makes the model change approach.
func TestCycleNoticeNamesTheBehaviour(t *testing.T) {
	notice := loop.CycleNotice("you have called the same tool with the same arguments")

	assert.Contains(t, notice, "same tool with the same arguments", "the specific behavior must survive into the notice")

	// an unattributed cycle still produces something actionable
	assert.Contains(t, loop.CycleNotice(""), "repeating", "a detail-less cycle notice must still be actionable")
}

func TestSettleNoticeNamesBothTerminalTools(t *testing.T) {
	notice := loop.SettleNotice()

	for _, tool := range []string{loop.SuccessTool, loop.FailureTool} {
		assert.Contains(t, notice, tool)
	}
}

func TestTerminalToolsAreWellFormed(t *testing.T) {
	tools := loop.TerminalTools()

	require.Len(t, tools, 2)

	byName := map[string]fantasy.AgentTool{}

	for _, tool := range tools {
		byName[tool.Info().Name] = tool
	}

	for name, required := range map[string]string{loop.SuccessTool: "summary", loop.FailureTool: "reason"} {
		tool, ok := byName[name]

		require.True(t, ok, "terminal tool %q missing", name)

		info := tool.Info()

		assert.NotEmpty(t, info.Description, "%s has no description; the model needs to know when to call it", name)

		_, ok = info.Parameters[required]
		assert.True(t, ok, "%s must accept a %q argument", name, required)

		assert.Len(t, info.Required, 1, "%s must require", name)
		assert.Equal(t, required, info.Required[0], "%s must require", name)
	}
}

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
	for reason, want := range map[loop.StopReason]int{
		loop.StopSettled:       0,
		loop.StopFailed:        1,
		loop.StopUnsettled:     1,
		loop.StopIterations:    1,
		loop.StopCalls:         1,
		loop.StopTime:          1,
		loop.StopContinuations: 1,
		loop.StopCycle:         1,
		loop.StopEmpty:         1,
		loop.StopAborted:       1,
		loop.StopError:         1,
	} {
		result := loop.Result{Reason: reason}

		assert.Equal(t, want, result.ExitCode(), "%s: exit code", reason)
	}
}
