package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/openzot/openzot/internal/loop"
)

// DigestStatus maps a run's stop reason and exit code to the one human word a
// digest shows. "done", "failed", or "canceled".
func DigestStatus(reason string, code int) string {
	switch reason {
	case string(loop.StopAborted):
		return "canceled"
	case string(loop.StopFailed):
		return "failed"
	}

	if code != 0 {
		return "failed"
	}

	return "done"
}

// Digest is the compact end-of-run report printed after a run concludes.
//
// It exists because the full-screen viewer runs in the alternate screen and
// takes its stats with it when it restores the terminal, so nothing tells an
// operator what the run spent or where its record is. The digest is a small,
// fixed block that survives on the main screen and carries both.
type Digest struct {
	// Status is the human-readable ending. "done", "failed", "canceled".
	Status string

	// Session is the log the run was appended to, empty when none was written.
	Session string

	// Iterations and Calls are the run's agentic rounds and total tool calls.
	Iterations int
	Calls      int

	// InputTokens and OutputTokens are the provider-billed totals for the run.
	InputTokens  int
	OutputTokens int

	// Message is the prose the ending carried - a success summary or a failure
	// reason. Rendered last, and only when present.
	Message string
}

// RenderDigest formats a Digest as an aligned two-column block.
//
// The shape is by design the simplest thing that is both readable and
// trivial to parse. One row per line, a single-word key, then the value as the
// rest of the line. A consumer splits each line on its first run of spaces -
// key left, value right - with no quoting or escaping to handle, because every
// key is one token and every value is free to contain spaces. No borders, no
// ANSI. A block that survives being piped through `grep` or `awk` unharmed.
//
// Empty fields are omitted rather than shown blank, so a run with no session
// simply has no session row.
func RenderDigest(d Digest) string {
	type row struct {
		key   string
		value string
	}

	var rows []row

	if d.Status != "" {
		rows = append(rows, row{"status", d.Status})
	}

	if d.Session != "" {
		rows = append(rows, row{"session", d.Session})
	}

	rows = append(rows,
		row{"iterations", strconv.Itoa(d.Iterations)},
		row{"calls", strconv.Itoa(d.Calls)},
		row{"input-tokens", strconv.Itoa(d.InputTokens)},
		row{"output-tokens", strconv.Itoa(d.OutputTokens)},
	)

	if m := strings.TrimSpace(d.Message); m != "" {
		// A multi-line message would break the one-row-per-line contract, so it is flattened. The digest is a
		// pointer to the full record, not the record itself.
		rows = append(rows, row{"message", strings.Join(strings.Fields(m), " ")})
	}

	width := 0
	for _, r := range rows {
		if len(r.key) > width {
			width = len(r.key)
		}
	}

	var b strings.Builder

	for _, r := range rows {
		fmt.Fprintf(&b, "%-*s  %s\n", width, r.key, r.value)
	}

	return b.String()
}
