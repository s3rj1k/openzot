package loop

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

// The corpus pins the cycle heuristics, the runaway guards and the thread fit
// against the implementation they were ported from. Each record is one call: a
// function, its arguments and the value it must return.
//
// The corpus was captured from an engine whose messages are open maps; zot's are
// typed. So each record is first read into loop.Message, and a record whose shape
// the typed model cannot express - a message field zot has no place for, a
// recorded usage, an option the port turned into a constant - is counted and
// skipped rather than bent to fit. The floors in TestCorpus fail the suite if the
// share that is checked shrinks, so skipping cannot quietly become the default.
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

	// buildThread records carry the behaviour of the callbacks they were given
	Callbacks []corpusCallback `json:"callbacks"`
}

type corpusCallback struct {
	Kind   string            `json:"kind"`
	Args   []json.RawMessage `json:"args"`
	Result json.RawMessage   `json:"result"`
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
// message maps. ok is false for anything it cannot express.

// argumentForms records how each arguments value was written in a record, as an
// object or as a string. The typed model holds arguments only as the string the
// provider sent, so a record that writes the same call both ways and expects the
// two to differ is one it cannot express.
type argumentForms map[string]string

func typedMessage(raw map[string]any, forms argumentForms) (Message, bool) {
	for key := range raw {
		switch key {
		case "type", "text", "meta":
		default:
			return Message{}, false
		}
	}

	var message Message

	if value, present := raw["type"]; present {
		kind, ok := value.(string)
		if !ok {
			return Message{}, false
		}

		message.Type = MessageType(kind)
	}

	if value, present := raw["text"]; present {
		text, ok := value.(string)
		if !ok {
			return Message{}, false
		}

		message.Text = text
	}

	if meta, present := raw["meta"]; present {
		activity, ok := typedActivity(meta, forms)
		if !ok {
			return Message{}, false
		}

		message.Activity = activity
	}

	return message, true
}

func typedActivity(meta any, forms argumentForms) (*Activity, bool) {
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

	// zot's activities always carry their arguments, and only a response carries
	// a result; a record that says otherwise is a shape the typed model cannot
	// tell apart from the ones it can
	arguments, present := function["arguments"]
	if !present {
		return nil, false
	}

	activity := &Activity{Kind: ActivityKind(kind), Name: name}

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

	if hasResult != (activity.Kind == ActivityResponse) {
		return nil, false
	}

	activity.Result = result

	return activity, true
}

func typedMessages(t *testing.T, raw json.RawMessage) ([]Message, bool) {
	t.Helper()

	var maps []map[string]any

	if err := json.Unmarshal(raw, &maps); err != nil {
		t.Fatalf("decode messages: %v", err)
	}

	messages := make([]Message, 0, len(maps))
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

// hasCycleOptions reports whether a cycle record tunes the heuristic; the port
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
	"hasRepeatedSuffix":       37,
	"hasRepeatedActivityTail": 16,
	"hasRepeatedResultRun":    20,
	"isThreadCyclic":          27,
	"describeThreadCycle":     3,
	"hasRepeatedTextRun":      14,
	"createRepetitionGuard":   68,
	"buildThread":             10,
}

// TestCorpus runs every seeded case the typed model can express.
func TestCorpus(t *testing.T) {
	corpus := loadCorpus(t)

	checked := map[string]int{}
	skipped := map[string]int{}

	for _, record := range corpus.Records {
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

// runRecord runs one record, reporting false when its shape is one the typed
// model cannot express.
func runRecord(t *testing.T, record corpusRecord) bool {
	t.Helper()

	switch record.Fn {
	case "hasRepeatedSuffix", "hasRepeatedActivityTail", "hasRepeatedResultRun", "isThreadCyclic", "describeThreadCycle":
		if hasCycleOptions(record.Args) {
			return false
		}

		messages, ok := typedMessages(t, record.Args[0])
		if !ok {
			return false
		}

		switch record.Fn {
		case "hasRepeatedSuffix":
			expectEqual(t, record.Fn, hasRepeatedSuffix(messages), expectBool(t, record))

		case "hasRepeatedActivityTail":
			expectEqual(t, record.Fn, hasRepeatedActivityTail(messages), expectBool(t, record))

		case "hasRepeatedResultRun":
			expectEqual(t, record.Fn, hasRepeatedResultRun(messages), expectBool(t, record))

		case "isThreadCyclic":
			expectEqual(t, record.Fn, describeCycle(messages) != "", expectBool(t, record))

		case "describeThreadCycle":
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

	case "buildThread":
		return runFitRecord(t, record)

	default:
		t.Fatalf("unhandled corpus function %q", record.Fn)
	}

	return true
}

func expectEqual(t *testing.T, fn string, got, want bool) {
	t.Helper()

	if got != want {
		t.Errorf("%s = %v, want %v", fn, got, want)
	}
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

// runFitRecord replays a buildThread record through fit. The record's token
// estimator is a recorded callback keyed by message, so the cost is read back out
// of it; the record's expected thread is compared on which messages were kept and
// what they cost together.
//
// fit does less than the function it replaced - it has no recorded-usage
// shortcut, no trimming of the message straddling the budget, and integer
// budgets - so a record that exercises any of those is skipped.
func runFitRecord(t *testing.T, record corpusRecord) bool {
	t.Helper()

	var raw map[string]any

	if err := json.Unmarshal(record.Args[0], &raw); err != nil {
		t.Fatalf("decode options: %v", err)
	}

	if raw["inclusive"] != nil || raw["tokenEstimationFunction"] != "@callback" {
		return false
	}

	budget, ok := wholeNumber(raw["maxTokens"])
	if !ok {
		return false
	}

	minKept := 0

	if value, present := raw["minMessages"]; present {
		if minKept, ok = wholeNumber(value); !ok || minKept < 0 {
			return false
		}
	}

	// the record's own bookkeeping fields ride on the messages; the typed model has
	// no place for them, and the cost is replayed from the callbacks instead
	var entries []any

	for _, entry := range raw["messages"].([]any) {
		if fields, ok := entry.(map[string]any); ok {
			entry = withoutKey(withoutKey(fields, "estimatedTokens"), "tokens")
		}

		entries = append(entries, entry)
	}

	messages, ok := typedMessages(t, mustMarshal(t, entries))
	if !ok {
		return false
	}

	// the callbacks are keyed by the message they priced; a record that priced one
	// message two ways has no single cost to replay
	costs := map[string]int{}

	for _, callback := range record.Callbacks {
		if callback.Kind != "tokenEstimation" {
			return false
		}

		var priced map[string]any

		var usage map[string]any

		if json.Unmarshal(callback.Args[0], &priced) != nil || json.Unmarshal(callback.Result, &usage) != nil {
			return false
		}

		tokens, ok := wholeNumber(usage["tokens"])
		if !ok || tokens < 0 {
			return false
		}

		message, ok := typedMessage(withoutKey(withoutKey(priced, "estimatedTokens"), "tokens"), nil)
		if !ok {
			return false
		}

		key := costKey(message)

		if previous, seen := costs[key]; seen && previous != tokens {
			return false
		}

		costs[key] = tokens
	}

	cost := func(message Message) int { return costs[costKey(message)] }

	got := fit(messages, budget, minKept, cost)

	var want struct {
		Messages []map[string]any `json:"messages"`
		Usage    struct {
			Tokens float64 `json:"tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(record.Expected, &want); err != nil {
		t.Fatalf("decode expected: %v", err)
	}

	total := 0

	for _, message := range got {
		total += cost(message)
	}

	if len(got) != len(want.Messages) || float64(total) != want.Usage.Tokens {
		t.Fatalf("fit kept %d messages costing %d, want %d costing %v", len(got), total, len(want.Messages), want.Usage.Tokens)
	}

	for index, message := range got {
		if text, _ := want.Messages[index]["text"].(string); text != message.Text {
			t.Errorf("kept message %d is %q, want %q", index, message.Text, text)
		}
	}

	return true
}

// costKey identifies a message for cost replay.
func costKey(message Message) string {
	return string(message.Type) + "\x00" + message.Text
}

func withoutKey(entry map[string]any, key string) map[string]any {
	kept := make(map[string]any, len(entry))

	for name, value := range entry {
		if name != key {
			kept[name] = value
		}
	}

	return kept
}

func wholeNumber(value any) (int, bool) {
	number, ok := value.(float64)
	if !ok || number != math.Trunc(number) {
		return 0, false
	}

	return int(number), true
}

func mustMarshal(t *testing.T, value any) json.RawMessage {
	t.Helper()

	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return encoded
}

func closeEnough(got, want float64) bool {
	return math.Abs(got-want) < 1e-9
}
