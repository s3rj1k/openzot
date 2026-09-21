package main

import (
	"fmt"
	"io"

	"github.com/s3rj1k/agent/internal/loop"
	"github.com/s3rj1k/agent/internal/outcome"
	"github.com/s3rj1k/agent/internal/render"
)

// digestStatus maps a run's stop reason and exit code to the one human word a
// digest shows. "done", "failed", or "canceled".
func digestStatus(reason string, code int) string {
	switch reason {
	case string(outcome.StopAborted):
		return "canceled"
	case string(outcome.StopFailed):
		return "failed"
	}

	if code != 0 {
		return "failed"
	}

	return "done"
}

// printDigest writes the end-of-run digest. The outcome, what the run spent,
// and - when the run was recorded - the session log it was appended to.
func printDigest(w io.Writer, sessionPath string, result *loop.Result) {
	digest := render.Digest{
		Status:       digestStatus(string(result.Reason), result.ExitCode()),
		Session:      sessionPath,
		Iterations:   result.Budget.Iterations,
		Calls:        result.Budget.Calls,
		InputTokens:  result.Budget.InputTokens,
		OutputTokens: result.Budget.OutputTokens,
		Message:      result.Message,
	}

	fmt.Fprintf(w, "\n%s", render.RenderDigest(digest))
}
