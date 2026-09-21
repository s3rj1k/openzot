package window

import (
	"encoding/json"

	"github.com/s3rj1k/agent/internal/conversation"
)

// Short-term memory is the context window, managed lazily. Nothing is summarized or lost, since the log keeps
// every message. From the soft mark the oldest message is forgotten per request, and at the hard mark as many
// as it takes to stay under it, so a request is never rejected for length.

const (
	// How many of the newest messages are never forgotten, so one
	// oversized fresh tool result cannot evict the turn that has to interpret it.
	keepNewest = 2
)

// Forget returns the new offset into messages below which everything is
// forgotten, given used - the estimated cost of the whole request - against a
// window. The parameter from is the offset already in force. It never moves backwards.
func Forget(messages []conversation.Message, from, used, window, soft, hard int, cost func(conversation.Message) int) int {
	if used < window*soft/100 {
		return from
	}

	hardMark := window * hard / 100
	dropped := false

	for from < len(messages)-keepNewest {
		if dropped && used < hardMark {
			break
		}

		used -= cost(messages[from])
		from++
		dropped = true
	}

	return from
}

// Cost is what a message is priced at for trimming, its text plus the tool call it carries. A call's cost
// lives in the activity, not the text, so pricing a request half by text would make a big write look free.
func Cost(message conversation.Message) int {
	text := message.Text

	if message.Activity != nil {
		if encoded, err := json.Marshal(message.Activity); err == nil {
			text += string(encoded)
		}
	}

	return EstimateTokens(text) + messageOverhead
}
