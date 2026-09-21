package loop_test

import (
	"encoding/json"
	"math"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openzot/openzot/internal/conversation"
	"github.com/openzot/openzot/internal/loop"
)

// The corpus pins the cycle heuristics and the runaway guards against the engine they were ported from, one call per record.
// It came from an engine with open-map messages, so each record is read into conversation.Message, and one the typed model
// cannot express is counted and skipped, with floors in TestCorpus failing if the checked share shrinks. Ids are digests.

type corpusFile struct {
	Records []corpusRecord `json:"records"`
}

type corpusRecord struct {
	ID       string            `json:"id"`
	Fn       string            `json:"fn"`
	Args     []json.RawMessage `json:"args"`
	Expected json.RawMessage   `json:"expected"`

	// createRepetitionGuard records are a session rather than a single call
	Pushes    []string `json:"pushes"`
	TrippedAt *int     `json:"trippedAt"`
}

func loadCorpus(t *testing.T) corpusFile {
	t.Helper()

	raw, err := os.ReadFile("testdata/corpus.json")
	require.NoError(t, err)

	var corpus corpusFile

	require.NoError(t, json.Unmarshal(raw, &corpus))

	require.NotEmpty(t, corpus.Records)

	return corpus
}

// Messages and activities the typed model can hold, read out of a record's
// message maps. Ok is false for anything it cannot express.

// argumentForms records how each arguments value was written in a record, as an object or a string. The typed model holds
// only the string the provider sent, so a record writing the same call both ways and expecting them to differ cannot be expressed.
type argumentForms map[string]string

func typedActivity(meta any, forms argumentForms) (*conversation.Activity, bool) {
	fields, ok := meta.(map[string]any)
	if !ok || len(fields) != 1 {
		return nil, false
	}

	raw, ok := fields["activity"].(map[string]any)
	if !ok {
		return nil, false
	}

	for key := range raw {
		if key != "type" && key != "function" {
			return nil, false
		}
	}

	kind, ok := raw["type"].(string)
	if !ok {
		return nil, false
	}

	function, ok := raw["function"].(map[string]any)
	if !ok {
		return nil, false
	}

	for key := range function {
		if key != "name" && key != "arguments" && key != "result" {
			return nil, false
		}
	}

	name, ok := function["name"].(string)
	if !ok {
		return nil, false
	}

	// Zot's activities always carry their arguments, and only a response carries a result. A record
	// that says otherwise is a shape the typed model cannot tell apart from the ones it can.
	arguments, present := function["arguments"]
	if !present {
		return nil, false
	}

	activity := &conversation.Activity{Kind: conversation.ActivityKind(kind), Name: name}

	form := "string"

	if text, isText := arguments.(string); isText {
		activity.Arguments = text
	} else {
		encoded, err := json.Marshal(arguments)
		if err != nil {
			return nil, false
		}

		activity.Arguments = string(encoded)
		form = "object"
	}

	if forms != nil {
		if seen, ok := forms[activity.Arguments]; ok && seen != form {
			return nil, false
		}

		forms[activity.Arguments] = form
	}

	result, hasResult := function["result"]

	if hasResult != (activity.Kind == conversation.ActivityResponse) {
		return nil, false
	}

	activity.Result = result

	return activity, true
}

func typedMessage(raw map[string]any, forms argumentForms) (conversation.Message, bool) {
	for key := range raw {
		switch key {
		case "type", "text", "meta":
		default:
			return conversation.Message{}, false
		}
	}

	var message conversation.Message

	if value, present := raw["type"]; present {
		kind, ok := value.(string)
		if !ok {
			return conversation.Message{}, false
		}

		message.Type = conversation.MessageType(kind)
	}

	if value, present := raw["text"]; present {
		text, ok := value.(string)
		if !ok {
			return conversation.Message{}, false
		}

		message.Text = text
	}

	if meta, present := raw["meta"]; present {
		activity, ok := typedActivity(meta, forms)
		if !ok {
			return conversation.Message{}, false
		}

		message.Activity = activity
	}

	return message, true
}

func typedMessages(t *testing.T, raw json.RawMessage) ([]conversation.Message, bool) {
	t.Helper()

	var maps []map[string]any

	require.NoError(t, json.Unmarshal(raw, &maps))

	messages := make([]conversation.Message, 0, len(maps))
	forms := argumentForms{}

	for _, entry := range maps {
		message, ok := typedMessage(entry, forms)
		if !ok {
			return nil, false
		}

		messages = append(messages, message)
	}

	return messages, true
}

// hasCycleOptions reports whether a cycle record tunes the heuristic. The port
// fixed those knobs, so such a record has nothing to run.
func hasCycleOptions(args []json.RawMessage) bool {
	if len(args) < 2 {
		return false
	}

	var raw map[string]any

	return json.Unmarshal(args[1], &raw) == nil && len(raw) > 0
}

func expectBool(t *testing.T, record corpusRecord) bool {
	t.Helper()

	var expected bool

	require.NoError(t, json.Unmarshal(record.Expected, &expected), "%s: expected a boolean", record.ID)

	return expected
}

// corpusFloors is the least share of each function's records the typed model must
// still be able to check, set from the first run of the port. A drop means the
// adapter (or the model) started rejecting records it used to run.
var corpusFloors = map[string]int{
	litHasRepeatedSuffix:       37,
	litHasRepeatedActivityTail: 16,
	litHasRepeatedResultRun:    20,
	litIsThreadCyclic:          27,
	litDescribeThreadCycle:     3,
	"hasRepeatedTextRun":       14,
	"createRepetitionGuard":    68,
}

func expectEqual(t *testing.T, fn string, got, want bool) {
	t.Helper()

	assert.Equal(t, want, got, "%s", fn)
}

func closeEnough(got, want float64) bool {
	return math.Abs(got-want) < 1e-9
}

func runGuardRecord(t *testing.T, record corpusRecord) {
	t.Helper()

	options := loop.GuardOptions{}

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

	guard := loop.NewRunawayGuard(options)

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

	var expected *loop.GuardReason

	if len(record.Expected) > 0 && string(record.Expected) != "null" {
		expected = &loop.GuardReason{}

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

	assert.True(t, closeEnough(got.UniqueRatio, expected.UniqueRatio))

	assert.True(t, closeEnough(got.HapaxRatio, expected.HapaxRatio))
}

// runRecord runs one record, reporting false when its shape is one the typed
// model cannot express.
func runRecord(t *testing.T, record corpusRecord) bool {
	t.Helper()

	switch record.Fn {
	case litHasRepeatedSuffix, litHasRepeatedActivityTail, litHasRepeatedResultRun, litIsThreadCyclic, litDescribeThreadCycle:
		if hasCycleOptions(record.Args) {
			return false
		}

		messages, ok := typedMessages(t, record.Args[0])
		if !ok {
			return false
		}

		switch record.Fn {
		case litHasRepeatedSuffix:
			expectEqual(t, record.Fn, loop.HasRepeatedSuffix(messages), expectBool(t, record))

		case litHasRepeatedActivityTail:
			expectEqual(t, record.Fn, loop.HasRepeatedActivityTail(messages), expectBool(t, record))

		case litHasRepeatedResultRun:
			expectEqual(t, record.Fn, loop.HasRepeatedResultRun(messages), expectBool(t, record))

		case litIsThreadCyclic:
			expectEqual(t, record.Fn, loop.DescribeCycle(messages) != "", expectBool(t, record))

		case litDescribeThreadCycle:
			var want *string

			require.NoError(t, json.Unmarshal(record.Expected, &want))

			got := loop.DescribeCycle(messages)

			if want == nil {
				want = new(string)
			}

			assert.Equal(t, *want, got)
		}

	case "hasRepeatedTextRun":
		var text string

		require.NoError(t, json.Unmarshal(record.Args[0], &text))

		options := loop.TextRunOptions{}

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

		expectEqual(t, record.Fn, loop.HasRepeatedTextRun(text, options), expectBool(t, record))

	case "createRepetitionGuard":
		runGuardRecord(t, record)

	default:
		require.FailNowf(t, "unhandled corpus function %q", record.Fn)
	}

	return true
}

// TestCorpus runs every seeded case the typed model can express.
func TestCorpus(t *testing.T) {
	corpus := loadCorpus(t)

	checked := map[string]int{}
	skipped := map[string]int{}

	for _, record := range corpus.Records {
		// the trimming these records pin is not what runs any more. The
		// conversation is forgotten lazily, see forget_test.go
		if record.Fn == "buildThread" {
			continue
		}

		ran := true

		t.Run(record.ID, func(t *testing.T) {
			ran = runRecord(t, record)
		})

		if ran {
			checked[record.Fn]++
		} else {
			skipped[record.Fn]++
		}
	}

	for fn, floor := range corpusFloors {
		t.Logf("%-26s checked %3d, skipped %3d", fn, checked[fn], skipped[fn])

		assert.GreaterOrEqual(t, checked[fn], floor)
	}
}
