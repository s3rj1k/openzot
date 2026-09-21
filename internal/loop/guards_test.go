package loop

import (
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every notice must be recognizable as an injected instruction rather than the
// model's own words - the cycle detector skips them by that prefix, so a notice
// without it would break loop detection.
func TestNoticesCarryThePrefix(t *testing.T) {
	notices := map[string]string{
		"cycle":      cycleNotice("you keep calling the same tool"),
		"settle":     settleNotice(),
		"truncation": truncationNotice(),
	}

	for name, notice := range notices {
		assert.True(t, strings.HasPrefix(notice, noticePrefix), "%s notice does not carry the prefix", name)

		assert.NotEmpty(t, strings.TrimSpace(strings.TrimPrefix(notice, noticePrefix)), "%s notice has no content", name)
	}
}

// A nudge that only says "you seem stuck" produces another lap. Naming the
// behavior is what makes the model change approach.
func TestCycleNoticeNamesTheBehaviour(t *testing.T) {
	notice := cycleNotice("you have called the same tool with the same arguments")

	assert.Contains(t, notice, "same tool with the same arguments", "the specific behavior must survive into the notice")

	// an unattributed cycle still produces something actionable
	assert.Contains(t, cycleNotice(""), "repeating", "a detail-less cycle notice must still be actionable")
}

func TestSettleNoticeNamesBothTerminalTools(t *testing.T) {
	notice := settleNotice()

	for _, tool := range []string{SuccessTool, FailureTool} {
		assert.Contains(t, notice, tool)
	}
}

func TestTerminalToolsAreWellFormed(t *testing.T) {
	tools := terminalTools()

	require.Len(t, tools, 2)

	byName := map[string]fantasy.AgentTool{}

	for _, tool := range tools {
		byName[tool.Info().Name] = tool
	}

	for name, required := range map[string]string{SuccessTool: "summary", FailureTool: "reason"} {
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
		assert.NotEmpty(t, cycleDetail(heuristic), "heuristic %q has no explanation for the model", heuristic)
	}

	detail := cycleDetail("something-new")
	assert.Empty(t, detail, "an unknown heuristic should fall back to the generic notice, got %q", detail)
}

// A caller scripting against zot tells success from everything else by the exit
// code. Only a run that settled is a success.
func TestExitCodeSeparatesSuccessFromEverythingElse(t *testing.T) {
	for reason, want := range map[StopReason]int{
		StopSettled:       0,
		StopFailed:        1,
		StopUnsettled:     1,
		StopIterations:    1,
		StopCalls:         1,
		StopTime:          1,
		StopContinuations: 1,
		StopCycle:         1,
		StopEmpty:         1,
		StopAborted:       1,
		StopError:         1,
	} {
		result := Result{Reason: reason}

		assert.Equal(t, want, result.ExitCode(), "%s: exit code", reason)
	}
}
