package testutils

import (
	"encoding/json"
	"math"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// CorpusRecord is one captured case of a fixture corpus, a function name with its arguments and what it returned.
type CorpusRecord struct {
	ID       string            `json:"id"`
	Fn       string            `json:"fn"`
	Args     []json.RawMessage `json:"args"`
	Expected json.RawMessage   `json:"expected"`

	// Pushes and TrippedAt describe a guard record, which is a session of text chunks rather than a single call.
	Pushes    []string `json:"pushes"`
	TrippedAt *int     `json:"trippedAt"`
}

// LoadCorpus reads the records of a corpus file and fails the test when there are none.
func LoadCorpus(t *testing.T, path string) []CorpusRecord {
	t.Helper()

	raw, err := os.ReadFile(path) //nolint:gosec // G304: the test names its own fixture
	require.NoError(t, err)

	var corpus struct {
		Records []CorpusRecord `json:"records"`
	}

	require.NoError(t, json.Unmarshal(raw, &corpus))

	require.NotEmpty(t, corpus.Records)

	return corpus.Records
}

// ExpectBool is what a record expects, which must be a boolean.
func ExpectBool(t *testing.T, record CorpusRecord) bool {
	t.Helper()

	var expected bool

	require.NoError(t, json.Unmarshal(record.Expected, &expected), "%s: expected a boolean", record.ID)

	return expected
}

// CloseEnough compares two floats the way a fixture that went through JSON can be trusted to.
func CloseEnough(got, want float64) bool {
	return math.Abs(got-want) < 1e-9
}
