package loop

import "encoding/json"

// messageCost is what a message is priced at for trimming: its text plus the
// tool call it carries.
//
// A tool call has almost all its cost outside the text - the name, arguments and
// result live in the activity - so a request half (no text at all) priced by its
// text would look free to the trimmer, and a write of a whole file would let a
// thread that "fits" be rejected.
func messageCost(message Message) int {
	text := message.Text

	if message.Activity != nil {
		if encoded, err := json.Marshal(message.Activity); err == nil {
			text += string(encoded)
		}
	}

	return estimateMessageTokens(text)
}

// fit returns the newest suffix of the conversation that fits within budget
// tokens, oldest first.
//
// It walks from the newest message backwards, which is what makes the fit exact:
// the budget is spent on the most recent context first, and the walk stops the
// moment the next message would not fit - or the moment the budget is reached,
// so a run of free messages behind an exactly-full thread is not dragged along.
// The newest minKept messages are kept whatever they cost. Without that floor one
// oversized message starves the whole turn.
//
// It counts tokens and nothing else. Keeping paired messages together (a
// tool-call request and its response, which a provider rejects if split) is
// Organize's job, and is done on the result rather than before trimming.
func fit(messages []Message, budget, minKept int, cost func(Message) int) []Message {
	start := len(messages)
	total := 0

	for index := len(messages) - 1; index >= 0; index-- {
		kept := len(messages)-index <= minKept
		tokens := cost(messages[index])

		if total+tokens > budget && !kept {
			break
		}

		total += tokens
		start = index

		if total >= budget && !kept {
			break
		}
	}

	return messages[start:]
}
