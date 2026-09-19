package loop

import (
	"strings"
	"testing"
)

// costs prices a message by the length of its text, so a test states its budget
// in plain numbers.
func costs(message Message) int { return len(message.Text) }

func texts(messages []Message) string {
	var out []string

	for _, message := range messages {
		out = append(out, message.Text)
	}

	return strings.Join(out, ",")
}

func thread(words ...string) []Message {
	messages := make([]Message, len(words))

	for i, word := range words {
		messages[i] = Message{Type: TypeUser, Text: word}
	}

	return messages
}

func TestFitKeepsTheNewestSuffixThatFits(t *testing.T) {
	tests := []struct {
		name    string
		input   []Message
		budget  int
		minKept int
		want    string
	}{
		{"everything fits", thread("aa", "bb", "cc"), 100, 0, "aa,bb,cc"},
		{"the oldest is dropped first", thread("aaaa", "bb", "cc"), 5, 0, "bb,cc"},
		{"a message that does not fit ends the walk", thread("a", "bbbbbb", "c"), 5, 0, "c"},
		{"reaching the budget exactly ends the walk", thread("", "bb", "cc"), 4, 0, "bb,cc"},
		{"free messages fit under the budget", thread("", "bb", "cc"), 5, 0, ",bb,cc"},
		{"the newest minKept survive an oversized message", thread("a", "bbbbbbbb"), 3, 1, "bbbbbbbb"},
		{"the floor does not keep what is older", thread("a", "bbbbbbbb", "c"), 3, 2, "bbbbbbbb,c"},
		{"an empty conversation", nil, 10, 2, ""},
		{"a floor larger than the conversation", thread("aaaa", "bbbb"), 1, 5, "aaaa,bbbb"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := texts(fit(test.input, test.budget, test.minKept, costs)); got != test.want {
				t.Errorf("fit kept %q, want %q", got, test.want)
			}
		})
	}
}

// Trimming must not reorder or copy: the result is the tail of the conversation
// it was given.
func TestFitReturnsTheTailInOrder(t *testing.T) {
	input := thread("one", "two", "three", "four")

	got := fit(input, 9, 0, costs)

	if texts(got) != "three,four" {
		t.Fatalf("fit kept %q", texts(got))
	}

	if &got[0] != &input[2] {
		t.Error("fit should slice the conversation rather than copy it")
	}
}

// A tool call carries its cost outside the text. Priced by text alone, a request
// half would look free and a large write would slip past the budget.
func TestMessageCostCountsTheToolCall(t *testing.T) {
	bare := Message{Type: TypeActivity}

	call := Message{Type: TypeActivity, Activity: &Activity{
		Kind: ActivityRequest, ID: "c1", Name: "write", Arguments: strings.Repeat("x", 900),
	}}

	if messageCost(call) <= messageCost(bare)+200 {
		t.Errorf("a request's arguments must be priced: %d vs %d", messageCost(call), messageCost(bare))
	}
}

// The heuristics must see what the engine actually records: a model repeating one
// call and getting one answer is a loop, however it words the turns in between.
func TestTheEnginesOwnActivitiesTriggerCycleDetection(t *testing.T) {
	var messages []Message

	for range 4 {
		messages = append(messages,
			activity(ActivityRequest, "c1", "shell", `{"cmd":"ls"}`, nil),
			activity(ActivityResponse, "c1", "shell", `{"cmd":"ls"}`, "same"),
		)
	}

	if got := describeCycle(messages); got == "" {
		t.Error("four identical call/result pairs must read as a cycle")
	}

	// a different answer each time is progress
	var polling []Message

	for _, answer := range []string{"a", "b", "c", "d"} {
		polling = append(polling,
			activity(ActivityRequest, "c1", "shell", `{"cmd":"ls"}`, nil),
			activity(ActivityResponse, "c1", "shell", `{"cmd":"ls"}`, answer),
		)
	}

	if got := describeCycle(polling); got == "repeated_result_run" || got == "repeated_activity_tail" {
		t.Errorf("polling an endpoint until it changes is not a loop, got %q", got)
	}
}
