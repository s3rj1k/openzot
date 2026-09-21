package cycle_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/testutils"
)

// Cases the corpus could not carry, and what replaced them. Anything the capture left out is a hole in the guarantee that
// this package answers as the original did, and an unwritten hole looks like a forgotten case. So each is enumerated here
// with the hand-written test that replaced it, and the suite checks that test still exists.

// notPortable is a captured case that cannot be expressed as corpus data.
type notPortable struct {
	// ID is the record it would have been, kept so a future capture can tell
	// this case was considered rather than missed.
	ID string

	// Replacement names the hand-written test covering it.
	Replacement string

	// Why explains what made it uncapturable and what the replacement asserts.
	Why string
}

// notPortableCases is the complete set. All three are one hazard, a value containing a reference cycle, which JSON cannot
// express. The question survives the port, since a malformed provider payload must not abort a run and a cycle check that
// panics is worse than none.
var notPortableCases = []notPortable{
	{
		ID:          "hasRepeatedSuffix/e577bc4f1b53",
		Replacement: "TestCycleCircularResult",
		Why: "Asserts the heuristic tolerates a self-referential payload. Go has no equivalent " +
			"hazard on this path - marshaling a cycle returns an error rather than recursing " +
			"forever - but the underlying question still applies: does a malformed tool result " +
			"abort the run. The replacement builds a result containing itself and asserts the " +
			"check answers instead of panicking.",
	},
	{
		ID:          "hasRepeatedResultRun/a5c8fa462b6e",
		Replacement: "TestRepeatedResultRunCircularResult",
		Why: "A tool result containing a cycle, reaching the same hazard through the result-run " +
			"heuristic rather than the suffix one. Hand-written for the same reason: the input " +
			"cannot be written down as JSON.",
	},
	{
		ID:          "hasRepeatedResultRun/a5c8fa462b6e#1",
		Replacement: "TestRepeatedResultRunCircularResult",
		Why: "The second call the same case makes on the same cyclic result, checking the guard is " +
			"still willing to answer after meeting one. Covered by the same replacement, which " +
			"pushes the cyclic result twice and asserts both calls return rather than hang.",
	},
}

// A replacement that no longer exists means the case was dropped while
// the record still reads as though it were handled.
func TestEveryNotPortableCaseHasItsReplacement(t *testing.T) {
	sources, err := filepath.Glob("*_test.go")
	require.NoError(t, err)

	var body strings.Builder

	for _, source := range sources {
		raw, err := os.ReadFile(source)
		require.NoError(t, err, "read %s", source)

		body.Write(raw)
	}

	tests := body.String()

	for _, entry := range notPortableCases {
		if entry.Replacement == "" {
			assert.Failf(t, "unexpected", "%s names no replacement test", entry.ID)

			continue
		}

		assert.Contains(t, tests, "func "+entry.Replacement+"(", "%s points at %s, which does not exist", entry.ID, entry.Replacement)
	}
}

// These records were left out of the capture. If one turns up in the corpus,
// either the export changed or the case was portable after all - and the
// hand-written stand-in is now a second, divergent copy of it.
func TestNotPortableCasesAreAbsentFromTheCorpus(t *testing.T) {
	records := testutils.LoadCorpus(t, "testdata/corpus.json")

	present := map[string]bool{}

	for _, record := range records {
		present[record.ID] = true
	}

	for _, entry := range notPortableCases {
		assert.False(t, present[entry.ID], "%s is in the corpus but recorded as not portable, so the hand-written %s duplicates it", entry.ID, entry.Replacement)
	}
}

// The reason is what makes the omission auditable later.
func TestEveryNotPortableCaseIsExplained(t *testing.T) {
	seen := map[string]bool{}

	for _, entry := range notPortableCases {
		assert.NotEmpty(t, entry.ID, "a case names no record")

		assert.False(t, seen[entry.ID], "%s is recorded twice", entry.ID)

		seen[entry.ID] = true

		assert.GreaterOrEqual(t, len(entry.Why), 120, "%s: reason is too thin to act on", entry.ID)
	}
}
