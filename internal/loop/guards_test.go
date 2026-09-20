package loop

import (
	"strings"
	"testing"

	"charm.land/fantasy"
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
		if !strings.HasPrefix(notice, noticePrefix) {
			t.Errorf("%s notice does not carry the prefix: %q", name, notice)
		}

		if strings.TrimSpace(strings.TrimPrefix(notice, noticePrefix)) == "" {
			t.Errorf("%s notice has no content", name)
		}
	}
}

// A nudge that only says "you seem stuck" produces another lap. Naming the
// behavior is what makes the model change approach.
func TestCycleNoticeNamesTheBehaviour(t *testing.T) {
	notice := cycleNotice("you have called the same tool with the same arguments")

	if !strings.Contains(notice, "same tool with the same arguments") {
		t.Errorf("the specific behavior must survive into the notice: %q", notice)
	}

	// an unattributed cycle still produces something actionable
	if got := cycleNotice(""); !strings.Contains(got, "repeating") {
		t.Errorf("a detail-less cycle notice must still be actionable: %q", got)
	}
}

func TestSettleNoticeNamesBothTerminalTools(t *testing.T) {
	notice := settleNotice()

	for _, tool := range []string{SuccessTool, FailureTool} {
		if !strings.Contains(notice, tool) {
			t.Errorf("the settle notice must name %s: %q", tool, notice)
		}
	}
}

func TestTerminalToolsAreWellFormed(t *testing.T) {
	tools := terminalTools()

	if len(tools) != 2 {
		t.Fatalf("got %d terminal tools, want 2", len(tools))
	}

	byName := map[string]fantasy.AgentTool{}

	for _, tool := range tools {
		byName[tool.Info().Name] = tool
	}

	for name, required := range map[string]string{SuccessTool: "summary", FailureTool: "reason"} {
		tool, ok := byName[name]

		if !ok {
			t.Fatalf("terminal tool %q missing", name)
		}

		info := tool.Info()

		if info.Description == "" {
			t.Errorf("%s has no description; the model needs to know when to call it", name)
		}

		if _, ok := info.Parameters[required]; !ok {
			t.Errorf("%s must accept a %q argument", name, required)
		}

		if len(info.Required) != 1 || info.Required[0] != required {
			t.Errorf("%s must require %q, got %v", name, required, info.Required)
		}
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
		if detail := cycleDetail(heuristic); detail == "" {
			t.Errorf("heuristic %q has no explanation for the model", heuristic)
		}
	}

	if detail := cycleDetail("something-new"); detail != "" {
		t.Errorf("an unknown heuristic should fall back to the generic notice, got %q", detail)
	}
}

// A caller scripting against zot tells success from everything else by the exit
// code: only a run that settled is a success.
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
		if got := (Result{Reason: reason}).ExitCode(); got != want {
			t.Errorf("%s: exit code %d, want %d", reason, got, want)
		}
	}
}
