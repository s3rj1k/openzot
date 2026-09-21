package outcome_test

import (
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/outcome"
)

// Every notice must be recognizable as an injected instruction rather than the
// model's own words - the cycle detector skips them by that prefix, so a notice
// without it would break loop detection.
func TestNoticesCarryThePrefix(t *testing.T) {
	notices := map[string]string{
		"cycle":      outcome.CycleNotice("you keep calling the same tool"),
		"settle":     outcome.SettleNotice(),
		"truncation": outcome.TruncationNotice(),
	}

	for name, notice := range notices {
		assert.True(t, strings.HasPrefix(notice, outcome.NoticePrefix), "%s notice does not carry the prefix", name)

		assert.NotEmpty(t, strings.TrimSpace(strings.TrimPrefix(notice, outcome.NoticePrefix)), "%s notice has no content", name)
	}
}

// A nudge that only says "you seem stuck" produces another lap. Naming the
// behavior is what makes the model change approach.
func TestCycleNoticeNamesTheBehaviour(t *testing.T) {
	notice := outcome.CycleNotice("you have called the same tool with the same arguments")

	assert.Contains(t, notice, "same tool with the same arguments", "the specific behavior must survive into the notice")

	// an unattributed cycle still produces something actionable
	assert.Contains(t, outcome.CycleNotice(""), "repeating", "a detail-less cycle notice must still be actionable")
}

func TestSettleNoticeNamesBothTerminalTools(t *testing.T) {
	notice := outcome.SettleNotice()

	for _, tool := range []string{outcome.SuccessTool, outcome.FailureTool} {
		assert.Contains(t, notice, tool)
	}
}

func TestTerminalToolsAreWellFormed(t *testing.T) {
	tools := outcome.TerminalTools()

	require.Len(t, tools, 2)

	byName := map[string]fantasy.AgentTool{}

	for _, tool := range tools {
		byName[tool.Info().Name] = tool
	}

	for name, required := range map[string]string{outcome.SuccessTool: "summary", outcome.FailureTool: "reason"} {
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
