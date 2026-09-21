package tui_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/loop"
	"github.com/openzot/openzot/internal/tui"
)

func TestModelRunErrorReportsFailedAgentExit(t *testing.T) {
	m := tui.NewModel("task", "model", "openai", "/tmp")
	m.Finish(&loop.Result{Reason: loop.StopFailed, Message: "verification failed"})

	err := m.RunError()
	require.Error(t, err, "want a failed agent exit")

	exitErr, ok := errors.AsType[*tui.AgentExitError](err)
	require.True(t, ok, "runError() = %T, want *AgentExitError", err)

	assert.Equal(t, 1, exitErr.Code, "want code 1 and the agent message")
	assert.Contains(t, exitErr.Error(), "verification failed", "want code 1 and the agent message")
}

func TestModelRunErrorReportsStreamError(t *testing.T) {
	want := errors.New("provider unavailable")
	m := tui.NewModel("task", "model", "openai", "/tmp")
	m.Err = want

	require.ErrorIs(t, m.RunError(), want)
}

func TestModelRunErrorRejectsEarlyViewerExit(t *testing.T) {
	m := tui.NewModel("task", "model", "openai", "/tmp")

	require.Error(t, m.RunError(), "runError() = nil while the agent is still running")
}

// The exit error is what the CLI prints and what the process status is derived
// from, so it has to read sensibly with and without an explanation.
func TestAgentExitErrorMessage(t *testing.T) {
	tests := []struct {
		name string
		err  *tui.AgentExitError
		want string
	}{
		{
			name: "with an explanation",
			err:  &tui.AgentExitError{Code: 2, Message: "could not build the project"},
			want: "agent exited with code 2: could not build the project",
		},
		{
			name: "without one",
			err:  &tui.AgentExitError{Code: 3},
			want: "agent exited with code 3",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, test.err.Error())
		})
	}
}
