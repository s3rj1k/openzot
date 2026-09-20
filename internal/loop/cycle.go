package loop

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/openzot/openzot/internal/conversation"
)

// The cycle heuristics decide whether a conversation has stopped making
// progress. They overlap deliberately - each covers a blind spot of the others -
// and the first to fire is the one reported, because a stuck run is otherwise
// only ever "stopped for looping" and the four fail in very different ways.
//
// Their behaviour was pinned against the implementation they were ported from;
// see corpus_test.go.

const (
	// cycleMinRepetitions is how many consecutive repeats of a pattern make a
	// cycle: [A B A B] is cyclic.
	cycleMinRepetitions = 2

	// cycleMinPatternLength is the shortest pattern considered, in messages. Two
	// catches the common ask/answer/ask/answer loop.
	cycleMinPatternLength = 2

	// cycleMinResultRepetitions is how many consecutive identical tool results
	// make a loop.
	cycleMinResultRepetitions = 3

	// cycleMinTail is how many trailing activity messages the activity-tail
	// heuristic needs before it will judge them.
	cycleMinTail = 8
)

// safeStringify renders a value as JSON for use as a comparison key, degrading
// to a sentinel rather than failing.
//
// Values reaching the heuristics include tool results, which can be anything -
// including structures that cannot be marshalled. A cycle check must never be
// the thing that aborts a run, so an unserialisable value collapses to a
// constant - which makes two such values compare equal, and is the intended
// trade: the alternative is no check at all.
func safeStringify(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `"[unserializable]"`
	}

	return string(encoded)
}

// hasRepeatedSuffix reports whether the conversation ends in a repeating block
// of messages - the plainest form of a loop, where the model and its context
// cycle through the same exchange verbatim.
//
// Messages are reduced to a fingerprint of (type, text, activity), and patterns
// are tried shortest-first because tight loops are both more common and more
// urgent than long ones.
//
// It requires the repeats to be byte-identical and adjacent, which is its
// blind spot: one interleaved message - a reasoning turn between tool calls -
// breaks the run. hasRepeatedResultRun covers that case.
func hasRepeatedSuffix(messages []conversation.Message) bool {
	if len(messages) < cycleMinPatternLength*cycleMinRepetitions {
		return false
	}

	fingerprints := make([]string, len(messages))

	for index, message := range messages {
		fingerprints[index] = safeStringify(message)
	}

	for length := cycleMinPatternLength; length <= len(messages)/cycleMinRepetitions; length++ {
		pattern := fingerprints[len(fingerprints)-length:]

		repetitions := 0

		for start := len(fingerprints) - length; start >= 0; start -= length {
			if !slices.Equal(fingerprints[start:start+length], pattern) {
				break
			}

			repetitions++
		}

		if repetitions >= cycleMinRepetitions {
			return true
		}
	}

	return false
}

// activityTailEntry is one tool-call activity reduced to its identity.
type activityTailEntry struct {
	kind   conversation.ActivityKind
	name   string
	input  string
	output string

	// hasOutput is whether the entry reports a result: a response does, a request
	// does not.
	hasOutput bool
}

// hasRepeatedActivityTail reports whether the conversation ends in a run of tool
// calls that keeps re-treading the same small set of signatures.
//
// It compresses the trailing contiguous activity messages to (kind, name, input,
// output) and asks whether that set has collapsed. Unlike hasRepeatedSuffix it
// tolerates the text around the calls varying, which catches a model that
// narrates differently each time while doing the same thing.
//
// A request whose repeats produce genuinely different outputs is spared: polling
// an endpoint until it changes is progress, not a loop.
func hasRepeatedActivityTail(messages []conversation.Message) bool {
	var tail []activityTailEntry

	for _, message := range slices.Backward(messages) {

		if message.Type != conversation.TypeActivity || message.Activity == nil {
			break
		}

		activity := message.Activity

		entry := activityTailEntry{
			kind:  activity.Kind,
			name:  activity.Name,
			input: safeStringify(activity.Arguments),
		}

		if activity.Kind == conversation.ActivityResponse {
			entry.output = safeStringify(activity.Output())
			entry.hasOutput = true
		}

		tail = append(tail, entry)
	}

	for left, right := 0, len(tail)-1; left < right; left, right = left+1, right-1 {
		tail[left], tail[right] = tail[right], tail[left]
	}

	if len(tail) < cycleMinTail {
		return false
	}

	var requestInputs, responseOutputs []string

	for _, entry := range tail {
		if entry.kind == conversation.ActivityRequest {
			requestInputs = append(requestInputs, entry.input)
		}

		if entry.kind == conversation.ActivityResponse {
			responseOutputs = append(responseOutputs, entry.output)
		}
	}

	if len(requestInputs) < 4 || len(responseOutputs) < 4 {
		return false
	}

	uniqueNames := map[string]struct{}{}
	uniqueSignatures := map[string]struct{}{}

	for _, entry := range tail {
		uniqueNames[entry.name] = struct{}{}

		var output any

		if entry.hasOutput {
			output = entry.output
		}

		uniqueSignatures[safeStringify([]any{entry.kind, entry.name, entry.input, output})] = struct{}{}
	}

	// pair each request with the response that follows it, so a request whose
	// answers keep changing can be recognised as progress

	outputSets := map[string]map[string]struct{}{}
	outputCounts := map[string]int{}

	for index := 0; index < len(tail)-1; index++ {
		request, response := tail[index], tail[index+1]

		if request.kind != conversation.ActivityRequest || response.kind != conversation.ActivityResponse || request.name != response.name {
			continue
		}

		key := safeStringify([]any{request.name, request.input})

		if outputSets[key] == nil {
			outputSets[key] = map[string]struct{}{}
		}

		outputSets[key][response.output] = struct{}{}
		outputCounts[key]++
	}

	progressing := false

	for key, outputs := range outputSets {
		if outputCounts[key] >= 3 && len(outputs) == outputCounts[key] {
			progressing = true

			break
		}
	}

	compressed := len(uniqueSignatures)*2 <= len(tail)

	return compressed &&
		len(uniqueNames) < len(tail) &&
		distinct(requestInputs) < len(requestInputs) &&
		distinct(responseOutputs) < len(responseOutputs) &&
		!progressing
}

func distinct(values []string) int {
	seen := make(map[string]struct{}, len(values))

	for _, value := range values {
		seen[value] = struct{}{}
	}

	return len(seen)
}

// hasRepeatedResultRun reports whether the last few tool results are identical -
// the model issuing the same call and getting the same answer, learning nothing.
//
// It walks only the tool-result stream, skipping every message in between. That
// is the gap it fills: hasRepeatedSuffix needs the surrounding messages to
// repeat byte-for-byte, and hasRepeatedActivityTail needs a contiguous run of
// activities, so a single interleaved reasoning message defeats both - and a
// reasoning model emits one between every tool call.
//
// Arguments are part of the signature. A model issuing different calls that
// happen to return the same trivial result (several distinct shell commands
// each producing empty output) is making progress, not looping.
//
// Synthetic "_"-prefixed activities are skipped, so a notice injected to nudge
// the model out of a loop cannot break the run and mask the loop.
func hasRepeatedResultRun(messages []conversation.Message) bool {
	var signatures []string

	for _, message := range slices.Backward(messages) {

		if message.Type != conversation.TypeActivity || message.Activity == nil {
			continue
		}

		activity := message.Activity

		if activity.Kind != conversation.ActivityResponse || strings.HasPrefix(activity.Name, "_") {
			continue
		}

		signatures = append(signatures, safeStringify([]any{
			activity.Name,
			safeStringify(activity.Arguments),
			safeStringify(activity.Output()),
		}))

		// measured from the newest backwards, so once the window is full the
		// answer is already determined
		if len(signatures) >= cycleMinResultRepetitions {
			break
		}
	}

	if len(signatures) < cycleMinResultRepetitions {
		return false
	}

	for _, signature := range signatures[1:] {
		if signature != signatures[0] {
			return false
		}
	}

	return true
}

// hasRepeatedMessageTextRun is the conversation-level adapter around
// hasRepeatedTextRun: a backstop for a degenerate message that reached the
// conversation despite the streaming guard.
//
// Reasoning and activity messages are exempt. The reasoning channel is the
// model's scratchpad and activity carries tool output; both are legitimately
// repetitive - enumerations, grids, table rows - and neither is the answer the
// user sees.
func hasRepeatedMessageTextRun(messages []conversation.Message) bool {
	start := max(len(messages)-5, 0)

	for _, message := range messages[start:] {
		if message.Type == conversation.TypeReasoning || message.Type == conversation.TypeActivity {
			continue
		}

		if hasRepeatedTextRun(message.Text, textRunOptions{}) {
			return true
		}
	}

	return false
}

// cycleHeuristics is ordered: the first to fire is the one reported.
var cycleHeuristics = []struct {
	name   string
	detect func([]conversation.Message) bool
}{
	{"repeated_suffix", hasRepeatedSuffix},
	{"repeated_activity_tail", hasRepeatedActivityTail},
	{"repeated_result_run", hasRepeatedResultRun},
	{"repeated_message_text_run", hasRepeatedMessageTextRun},
}

// describeCycle names the first heuristic to fire, or returns the empty string
// when the conversation is progressing.
func describeCycle(messages []conversation.Message) string {
	for _, heuristic := range cycleHeuristics {
		if heuristic.detect(messages) {
			return heuristic.name
		}
	}

	return ""
}
