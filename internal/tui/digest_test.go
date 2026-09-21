package tui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/loop"
)

func TestRenderDigestIsColumnarAndParsable(t *testing.T) {
	out := RenderDigest(Digest{
		Status:       litDone,
		Session:      "20260824-143210",
		Iterations:   42,
		Calls:        137,
		InputTokens:  1234567,
		OutputTokens: 84210,
		Message:      "Found 3 auth bypasses.",
	})

	// Every non-empty line must parse as (key, value) by splitting on the first
	// run of spaces - the whole point of the format.
	got := map[string]string{}

	for line := range strings.SplitSeq(strings.TrimRight(out, "\n"), "\n") {
		key, value, found := strings.Cut(strings.TrimRight(line, " "), " ")
		require.True(t, found, "line %q is not a key/value pair", line)

		got[key] = strings.TrimSpace(value)
	}

	want := map[string]string{
		"status":        litDone,
		"session":       "20260824-143210",
		"iterations":    "42",
		"calls":         "137",
		"input-tokens":  "1234567",
		"output-tokens": "84210",
		"message":       "Found 3 auth bypasses.",
	}

	for k, v := range want {
		assert.Equal(t, v, got[k])
	}
}

func TestRenderDigestOmitsEmptyFields(t *testing.T) {
	out := RenderDigest(Digest{Status: litDone, Iterations: 1, Calls: 1})

	for _, absent := range []string{"session", "message"} {
		assert.NotContains(t, out, absent, "a run with no %s must not render that row", absent)
	}
}

func TestRenderDigestFlattensMultilineMessage(t *testing.T) {
	out := RenderDigest(Digest{Status: litDone, Message: "line one\nline two"})

	// The one-row-per-line contract must hold even for a multi-line message.
	assert.NotContains(t, out, "line one\nline two", "a multi-line message must be flattened to one line")

	assert.Contains(t, out, "line one line two", "the message must survive flattening")
}

func TestDigestStatus(t *testing.T) {
	cases := []struct {
		reason string
		code   int
		want   string
	}{
		{string(loop.StopSettled), 0, litDone},
		{string(loop.StopFailed), 1, litFailed},
		{string(loop.StopAborted), 1, "canceled"},
		{string(loop.StopIterations), 3, litFailed},
	}

	for _, c := range cases {
		assert.Equal(t, c.want, DigestStatus(c.reason, c.code))
	}
}
