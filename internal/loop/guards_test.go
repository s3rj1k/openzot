package loop

import (
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestNormalizeCheckpoints(t *testing.T) {
	// nil means "use the defaults"; an empty (non-nil) slice means "disabled"
	if got := normalizeCheckpoints(nil); len(got) == 0 {
		t.Error("nil checkpoints must fall back to the defaults")
	}

	if got := normalizeCheckpoints([]int{}); len(got) != 0 {
		t.Errorf("an explicit empty list must disable checkpoints, got %v", got)
	}

	// sorted, deduped, and clamped to 1..99 (0 is no progress, 100 is the stop)
	got := normalizeCheckpoints([]int{90, 50, 50, 0, 100, 150, 80})

	want := []int{50, 80, 90}
	if len(got) != len(want) {
		t.Fatalf("normalizeCheckpoints = %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("normalizeCheckpoints = %v, want %v", got, want)

			break
		}
	}
}

// Every notice must be recognisable as an injected instruction rather than the
// model's own words - the cycle detector skips them by that prefix, so a notice
// without it would break loop detection.
func TestNoticesCarryThePrefix(t *testing.T) {
	notices := map[string]string{
		"cycle":      cycleNotice("you keep calling the same tool"),
		"settle":     settleNotice(),
		"checkpoint": limitCheckpointNotice(iterationLimit, 80, "8 of 10"),
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
// behaviour is what makes the model change approach.
func TestCycleNoticeNamesTheBehaviour(t *testing.T) {
	notice := cycleNotice("you have called the same tool with the same arguments")

	if !strings.Contains(notice, "same tool with the same arguments") {
		t.Errorf("the specific behaviour must survive into the notice: %q", notice)
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

func TestLimitCheckpointNoticeStatesTheProgress(t *testing.T) {
	got := limitCheckpointNotice(toolCallLimit, 80, "40 of 50")

	// the model needs the percentage and the concrete usage, in units it can act
	// on, to pace itself
	for _, want := range []string{"80%", "tool-call", "40 of 50 tool calls"} {
		if !containsFold(got, want) {
			t.Errorf("the checkpoint notice must mention %q: %q", want, got)
		}
	}
}

// The guidance must scale with how close the limit is. "Finish now" at the
// halfway mark would make the model quit with half its budget unused, which is
// worse than saying nothing; "keep an eye on it" near the end is too weak.
func TestCheckpointGuidanceScalesWithUrgency(t *testing.T) {
	early := limitCheckpointNotice(toolCallLimit, 50, "25 of 50")
	near := limitCheckpointNotice(toolCallLimit, 90, "45 of 50")

	// at the halfway mark: awareness, not a demand to stop
	if containsFold(early, "close to the limit") || containsFold(early, "stop and complete") {
		t.Errorf("a 50%% notice must not tell the model to finish now: %q", early)
	}

	if !containsFold(early, "budget left") && !containsFold(early, "pace yourself") {
		t.Errorf("a 50%% notice should be a gentle heads-up: %q", early)
	}

	// near the end: an actual instruction to finish
	if !containsFold(near, "close to the limit") || !containsFold(near, "now") {
		t.Errorf("a 90%% notice must push the model to finish: %q", near)
	}

	// the two must genuinely differ
	if early == near {
		t.Error("the halfway and near-limit notices must not be identical")
	}
}

// The three limits must read distinctly - a heads-up about tool calls must not
// be mistakable for one about time - and each must carry advice suited to it,
// so the model knows which constraint it is up against and what to do.
func TestEachLimitReadsDistinctly(t *testing.T) {
	iter := limitCheckpointNotice(iterationLimit, 80, "8 of 10")
	call := limitCheckpointNotice(toolCallLimit, 80, "40 of 50")
	time := limitCheckpointNotice(timeLimit, 80, "1m30s of 2m0s")

	// each names its own limit
	if !containsFold(iter, "step budget") || !containsFold(call, "tool-call budget") || !containsFold(time, "time budget") {
		t.Error("each notice must name its own limit")
	}

	// and none reads like another - the three are genuinely different messages
	if iter == call || call == time || iter == time {
		t.Error("the three limit notices must not be identical")
	}

	// the units are the ones the model counts in
	if !containsFold(iter, "steps") || !containsFold(call, "tool calls") {
		t.Errorf("usage must carry units:\n%s\n%s", iter, call)
	}

	// and the advice names what wastes this particular budget (at the 80%
	// prioritising band), which time - spent by the clock, not an action - omits
	if !containsFold(call, "redundant") {
		t.Errorf("the tool-call notice should name redundant calls as the waste to avoid: %q", call)
	}

	if containsFold(time, "redundant") || containsFold(time, "investigation") {
		t.Errorf("the time notice has no controllable waste to name: %q", time)
	}
}

func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
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
