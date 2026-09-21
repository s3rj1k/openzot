package runaway_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/runaway"
	"github.com/openzot/openzot/internal/testutils"
)

// The corpus pins the runaway guard against the engine it was ported from. A text-run record is a single call, and a
// guard record is a session of text chunks with the point the guard tripped. Ids are digests.

// corpusFloors is the least share of each function's records that must still be checked, set from the first run of the
// port. A drop means the adapter started rejecting records it used to run.
var corpusFloors = map[string]int{
	"hasRepeatedTextRun":    14,
	"createRepetitionGuard": 68,
}

func expectEqual(t *testing.T, fn string, got, want bool) {
	t.Helper()

	assert.Equal(t, want, got, "%s", fn)
}

func runGuardRecord(t *testing.T, record testutils.CorpusRecord) {
	t.Helper()

	options := runaway.GuardOptions{}

	if len(record.Args) > 0 {
		var raw struct {
			Ngram          *int     `json:"ngram"`
			Window         *int     `json:"window"`
			MaxRepeats     *int     `json:"maxRepeats"`
			MaxUniqueRatio *float64 `json:"maxUniqueRatio"`
			MinChars       *int     `json:"minChars"`
		}

		if err := json.Unmarshal(record.Args[0], &raw); err == nil {
			options.Ngram = raw.Ngram
			options.Window = raw.Window
			options.MaxRepeats = raw.MaxRepeats
			options.MaxUniqueRatio = raw.MaxUniqueRatio
			options.MinChars = raw.MinChars
		}
	}

	guard := runaway.NewGuard(options)

	trippedAt := -1

	for index, chunk := range record.Pushes {
		if guard.Push(chunk) && trippedAt < 0 {
			trippedAt = index
		}
	}

	want := -1

	if record.TrippedAt != nil {
		want = *record.TrippedAt
	}

	require.Equal(t, want, trippedAt)

	var expected *runaway.GuardReason

	if len(record.Expected) > 0 && string(record.Expected) != "null" {
		expected = &runaway.GuardReason{}

		require.NoError(t, json.Unmarshal(record.Expected, expected))
	}

	got := guard.Reason()

	switch {
	case expected == nil && got != nil:
		require.FailNowf(t, "want no reason", "got %+v", *got)
	case expected == nil:
		return
	case got == nil:
		require.FailNowf(t, "want a reason", "expected %+v", *expected)
	}

	assert.Equal(t, expected.Phrase, got.Phrase)
	assert.Equal(t, expected.Count, got.Count)
	assert.Equal(t, expected.Text, got.Text)

	assert.True(t, testutils.CloseEnough(got.UniqueRatio, expected.UniqueRatio))

	assert.True(t, testutils.CloseEnough(got.HapaxRatio, expected.HapaxRatio))
}

// runRecord runs one record.
func runRecord(t *testing.T, record testutils.CorpusRecord) {
	t.Helper()

	switch record.Fn {
	case "hasRepeatedTextRun":
		var text string

		require.NoError(t, json.Unmarshal(record.Args[0], &text))

		options := runaway.TextRunOptions{}

		if len(record.Args) > 1 {
			var raw struct {
				MinUnits       *int     `json:"minUnits"`
				Window         *int     `json:"window"`
				MaxUniqueRatio *float64 `json:"maxUniqueRatio"`
			}

			if err := json.Unmarshal(record.Args[1], &raw); err == nil {
				options.MinUnits = raw.MinUnits
				options.Window = raw.Window
				options.MaxUniqueRatio = raw.MaxUniqueRatio
			}
		}

		expectEqual(t, record.Fn, runaway.HasRepeatedTextRun(text, options), testutils.ExpectBool(t, record))

	case "createRepetitionGuard":
		runGuardRecord(t, record)

	default:
		require.FailNowf(t, "unhandled corpus function %q", record.Fn)
	}
}

// TestCorpus runs every seeded case.
func TestCorpus(t *testing.T) {
	checked := map[string]int{}

	for _, record := range testutils.LoadCorpus(t, "testdata/corpus.json") {
		t.Run(record.ID, func(t *testing.T) {
			runRecord(t, record)
		})

		checked[record.Fn]++
	}

	for fn, floor := range corpusFloors {
		t.Logf("%-26s checked %3d", fn, checked[fn])

		assert.GreaterOrEqual(t, checked[fn], floor)
	}
}
