package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/openzot/openzot/internal/loop"
)

func TestModelRunErrorReportsFailedAgentExit(t *testing.T) {
	m := newModel("task", "model", "openai", "/tmp")
	m.finish(loop.Result{Reason: loop.StopFailed, Message: "verification failed"})

	err := m.runError()
	if err == nil {
		t.Fatal("runError() = nil, want a failed agent exit")
	}

	exitErr, ok := errors.AsType[*AgentExitError](err)
	if !ok {
		t.Fatalf("runError() = %T, want *AgentExitError", err)
	}
	if exitErr.Code != 1 || !strings.Contains(exitErr.Error(), "verification failed") {
		t.Errorf("exit error = %+v, want code 1 and the agent message", exitErr)
	}
}

func TestModelRunErrorReportsStreamError(t *testing.T) {
	want := errors.New("provider unavailable")
	m := newModel("task", "model", "openai", "/tmp")
	m.err = want

	if got := m.runError(); !errors.Is(got, want) {
		t.Errorf("runError() = %v, want %v", got, want)
	}
}

func TestModelRunErrorRejectsEarlyViewerExit(t *testing.T) {
	m := newModel("task", "model", "openai", "/tmp")

	if err := m.runError(); err == nil {
		t.Fatal("runError() = nil while the agent is still running")
	}
}

// The exit error is what the CLI prints and what the process status is derived
// from, so it has to read sensibly with and without an explanation.
func TestAgentExitErrorMessage(t *testing.T) {
	tests := []struct {
		name string
		err  *AgentExitError
		want string
	}{
		{
			name: "with an explanation",
			err:  &AgentExitError{Code: 2, Message: "could not build the project"},
			want: "agent exited with code 2: could not build the project",
		},
		{
			name: "without one",
			err:  &AgentExitError{Code: 3},
			want: "agent exited with code 3",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.err.Error(); got != test.want {
				t.Errorf("Error() = %q, want %q", got, test.want)
			}
		})
	}
}
