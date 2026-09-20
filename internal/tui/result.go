package tui

import (
	"errors"
	"fmt"
)

// ErrCancelled reports that the operator closed the viewer while the run was
// still going. A sentinel, so a caller can tell a deliberate stop from a failure and
// report it calmly.
var ErrCancelled = errors.New("run canceled - the viewer was closed while the run was still going")

// AgentExitError reports an agent-declared failed run to callers so the CLI can
// return a non-zero process status.
type AgentExitError struct {
	Code    int
	Message string
}

func (e *AgentExitError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("agent exited with code %d", e.Code)
	}

	return fmt.Sprintf("agent exited with code %d: %s", e.Code, e.Message)
}

func (m model) runError() error {
	if m.err != nil {
		return m.err
	}

	if m.exitCode != 0 {
		return &AgentExitError{Code: m.exitCode, Message: m.exitMsg}
	}

	if m.status == statusRunning {
		// The viewer closed while the run was still going, and in production
		// only the operator does that - q or Ctrl-C. Say so: "ended before
		// completion" read as a mysterious failure when it was a keypress.
		return ErrCancelled
	}

	return nil
}
