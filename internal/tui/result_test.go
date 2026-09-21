package tui

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/loop"
)

func TestModelRunErrorReportsFailedAgentExit(t *testing.T) {
	m := newModel("task", "model", "openai", "/tmp")
	m.finish(&loop.Result{Reason: loop.StopFailed, Message: "verification failed"})

	err := m.runError()
	require.Error(t, err, "want a failed agent exit")

	exitErr, ok := errors.AsType[*AgentExitError](err)
	require.True(t, ok, "runError() = %T, want *AgentExitError", err)

	assert.Equal(t, 1, exitErr.Code, "want code 1 and the agent message")
	assert.Contains(t, exitErr.Error(), "verification failed", "want code 1 and the agent message")
}

func TestModelRunErrorReportsStreamError(t *testing.T) {
	want := errors.New("provider unavailable")
	m := newModel("task", "model", "openai", "/tmp")
	m.err = want

	require.ErrorIs(t, m.runError(), want)
}

func TestModelRunErrorRejectsEarlyViewerExit(t *testing.T) {
	m := newModel("task", "model", "openai", "/tmp")

	require.Error(t, m.runError(), "runError() = nil while the agent is still running")
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
			assert.Equal(t, test.want, test.err.Error())
		})
	}
}
