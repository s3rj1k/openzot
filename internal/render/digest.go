// Package render turns what a run does into text for the terminal. Tool calls and results, the token and time formats,
// the end-of-run digest and the shared styles, none of it holding any state of the viewer.
package render

import (
	"fmt"
	"strconv"
	"strings"
)

// Digest is the compact end-of-run report printed after a run concludes. The viewer takes its stats with it when it
// leaves the alternate screen, so this small fixed block survives on the main screen with what the run spent and where
// its record is.
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

// RenderDigest formats a Digest as an aligned two-column block, one row per line with a single-word key and the value as
// the rest of the line, so it survives grep and awk with no quoting. No borders, no ANSI. Empty fields are omitted,
// so a run with no session has no session row.
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
