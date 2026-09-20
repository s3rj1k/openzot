package loop

import (
	"encoding/json"
	"math"
	"os"
	"testing"

	"github.com/openzot/openzot/internal/conversation"
)

// The corpus pins the cycle heuristics and the runaway guards against the
// implementation they were ported from. Each record is one call. A
// function, its arguments and the value it must return.
//
// The corpus was captured from an engine whose messages are open maps. Zot's are
// typed. So each record is first read into conversation.Message, and a record whose shape
// the typed model cannot express - a message field zot has no place for, a
// recorded usage, an option the port turned into a constant - is counted and
// skipped rather than bent to fit. The floors in TestCorpus fail the suite if the
// share that is checked shrinks, so skipping cannot become the default.
//
// Ids are digests rather than names on purpose - the corpus is published and the
// suite it came from is not. To trace a failing id, look it up in the private
// provenance map. See docs/rfcs/zot-native-agent-engine.md.

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
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}

	var corpus corpusFile

	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("parse corpus: %v", err)
	}

	if len(corpus.Records) == 0 {
		t.Fatal("corpus is empty")
	}

	return corpus
}

// Messages and activities the typed model can hold, read out of a record's
// message maps. Ok is false for anything it cannot express.

// argumentForms records how each arguments value was written in a record, as an
// object or as a string. The typed model holds arguments only as the string the
// provider sent, so a record that writes the same call both ways and expects the
// two to differ is one it cannot express.
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

	/*
		zot's activities always carry their arguments, and only a response carries
		a result. A record that says otherwise is a shape the typed model cannot
		tell apart from the ones it can
	*/
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

	if err := json.Unmarshal(raw, &maps); err != nil {
		t.Fatalf("decode messages: %v", err)
	}

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

	if err := json.Unmarshal(record.Expected, &expected); err != nil {
		t.Fatalf("%s: expected a boolean: %v", record.ID, err)
	}

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

	if got != want {
		t.Errorf("%s = %v, want %v", fn, got, want)
	}
}

func closeEnough(got, want float64) bool {
	return math.Abs(got-want) < 1e-9
}

func runGuardRecord(t *testing.T, record corpusRecord) {
	t.Helper()

	options := guardOptions{}

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

	guard := newRunawayGuard(options)

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

	if trippedAt != want {
		t.Fatalf("tripped at %d, want %d", trippedAt, want)
	}

	var expected *guardReason

	if len(record.Expected) > 0 && string(record.Expected) != "null" {
		expected = &guardReason{}

		if err := json.Unmarshal(record.Expected, expected); err != nil {
			t.Fatalf("decode reason: %v", err)
		}
	}

	got := guard.Reason()

	switch {
	case expected == nil && got != nil:
		t.Fatalf("reason = %+v, want none", *got)
	case expected == nil:
		return
	case got == nil:
		t.Fatalf("reason = none, want %+v", *expected)
	}

	if got.Phrase != expected.Phrase || got.Count != expected.Count || got.Text != expected.Text {
		t.Errorf("reason = %+v, want %+v", *got, *expected)
	}

	if !closeEnough(got.UniqueRatio, expected.UniqueRatio) {
		t.Errorf("uniqueRatio = %v, want %v", got.UniqueRatio, expected.UniqueRatio)
	}

	if !closeEnough(got.HapaxRatio, expected.HapaxRatio) {
		t.Errorf("hapaxRatio = %v, want %v", got.HapaxRatio, expected.HapaxRatio)
	}
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
			expectEqual(t, record.Fn, hasRepeatedSuffix(messages), expectBool(t, record))

		case litHasRepeatedActivityTail:
			expectEqual(t, record.Fn, hasRepeatedActivityTail(messages), expectBool(t, record))

		case litHasRepeatedResultRun:
			expectEqual(t, record.Fn, hasRepeatedResultRun(messages), expectBool(t, record))

		case litIsThreadCyclic:
			expectEqual(t, record.Fn, describeCycle(messages) != "", expectBool(t, record))

		case litDescribeThreadCycle:
			var want *string

			if err := json.Unmarshal(record.Expected, &want); err != nil {
				t.Fatalf("decode expected: %v", err)
			}

			got := describeCycle(messages)

			if want == nil {
				want = new(string)
			}

			if got != *want {
				t.Errorf("describeCycle = %q, want %q", got, *want)
			}
		}

	case "hasRepeatedTextRun":
		var text string

		if err := json.Unmarshal(record.Args[0], &text); err != nil {
			t.Fatalf("decode text: %v", err)
		}

		options := textRunOptions{}

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

		expectEqual(t, record.Fn, hasRepeatedTextRun(text, options), expectBool(t, record))

	case "createRepetitionGuard":
		runGuardRecord(t, record)

	default:
		t.Fatalf("unhandled corpus function %q", record.Fn)
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

		if checked[fn] < floor {
			t.Errorf("%s: %d records checked, want at least %d", fn, checked[fn], floor)
		}
	}
}
