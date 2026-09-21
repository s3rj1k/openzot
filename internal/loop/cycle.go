package loop

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/openzot/openzot/internal/conversation"
)

// The cycle heuristics decide whether a conversation has stopped making progress. They overlap by design, each covering
// a blind spot of the others, and the first to fire is the one reported, since the four fail in very different ways.
// Their behavior is pinned by the corpus (see corpus_test.go).

const (
	// How many consecutive repeats of a pattern make a
	// cycle. [A B A B] is cyclic.
	cycleMinRepetitions = 2

	// The shortest pattern considered, in messages. Two
	// catches the common ask/answer/ask/answer loop.
	cycleMinPatternLength = 2

	// How many consecutive matching tool results
	// make a loop.
	cycleMinResultRepetitions = 3

	// How many trailing activity messages the activity-tail
	// heuristic needs before it will judge them.
	cycleMinTail = 8
)

// safeStringify renders a value as JSON for use as a comparison key, degrading to a sentinel rather than failing. A cycle
// check must never abort a run, so a tool result that cannot be marshaled collapses to a constant, making two such values
// compare equal. That is the intended trade, since the alternative is no check at all.
func safeStringify(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `"[unserializable]"`
	}

	return string(encoded)
}

// hasRepeatedSuffix reports whether the conversation ends in a repeating block of messages, the plainest form of a loop.
// Messages are fingerprinted by (type, text, activity) and patterns tried shortest-first, since tight loops are more common
// and urgent. The repeats must be byte-for-byte the same and adjacent, so one interleaved reasoning message defeats it.
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

	// hasOutput is whether the entry reports a result. A response does, a request
	// does not.
	hasOutput bool
}

func distinct(values []string) int {
	seen := make(map[string]struct{}, len(values))

	for _, value := range values {
		seen[value] = struct{}{}
	}

	return len(seen)
}

// hasRepeatedActivityTail reports whether the conversation ends in tool calls that keep re-treading the same small set of
// signatures. It compresses the trailing activities to (kind, name, input, output), so it tolerates varying text around the
// calls. A request whose repeats produce really different outputs is spared, since polling until it changes is progress.
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
	// answers keep changing can be recognized as progress

	outputSets := map[string]map[string]struct{}{}
	outputCounts := map[string]int{}

	for index := range len(tail) - 1 {
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

// hasRepeatedResultRun reports whether the last few tool results are the same, the model issuing the same call and learning
// nothing. It walks only the tool results, so an interleaved reasoning message cannot defeat it. Arguments are part of the
// signature, and synthetic "_"-prefixed activities are skipped so an injected notice cannot mask the loop.
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

// hasRepeatedMessageTextRun is the conversation-level fallback for a degenerate message that got past the streaming guard.
// Reasoning and activity messages are exempt, since the scratchpad and tool output are properly repetitive (enumerations,
// grids, table rows) and neither is the answer the user sees.
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

// cycleHeuristics is ordered. The first to fire is the one reported.
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
