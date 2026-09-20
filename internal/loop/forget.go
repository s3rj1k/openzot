package loop

import "encoding/json"

// Short-term memory is the context window, and it is managed lazily. Nothing is
// summarised and nothing is thrown away for good - the session log keeps every
// message - the request just stops carrying the oldest ones.
//
// Below the soft mark the whole conversation is sent. From the soft mark the
// oldest message is forgotten on each request, so the conversation may still
// drift upward between drops; at the hard mark as many go as it takes to be
// under it. The soft zone is what keeps compaction cheap and gradual, the hard
// mark is what keeps a request from ever being rejected for length.

const (
	// keepNewest is how many of the newest messages are never forgotten, so one
	// oversized fresh tool result cannot evict the turn that has to interpret it.
	keepNewest = 2

	// narrowFloor is the share of the configured window a rejection can narrow
	// the effective window down to, as a divisor: a provider that keeps saying
	// "too long" is wrong about its own ceiling only so far.
	narrowFloor = 4
)

// forget returns the new offset into messages below which everything is
// forgotten, given used - the estimated cost of the whole request - against a
// window. from is the offset already in force; it never moves backwards.
func forget(messages []Message, from, used, window, soft, hard int, cost func(Message) int) int {
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
