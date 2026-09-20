// Package conversation is what a run says and does, as data: the messages, the
// tool calls they carry, the repair that keeps a history valid to send, the
// rendering of it into a model prompt, and the forgetting that keeps it inside a
// context window. It knows nothing of the engine that drives a conversation or
// the provider that answers it.
package conversation

// MessageType identifies what a message is.
//
// A named type rather than a bare string: these values decide how a message is
// rendered to the provider and whether the runaway backstop scans it. A typo in
// a string literal would silently route a message down the wrong path - a system
// prompt rendered as ordinary history, say - and nothing would report it.
type MessageType string

// The message types the loop understands.
const (
	// TypeUser is input from the operator, and the channel the loop injects its
	// own notices on.
	TypeUser MessageType = "user"

	// TypeBot is the model's answer.
	TypeBot MessageType = "bot"

	// TypeReasoning is the model's scratchpad. Never replayed to the provider,
	// and exempt from the runaway-text backstop.
	TypeReasoning MessageType = "reasoning"

	// TypeActivity is one half of a tool-call pair.
	TypeActivity MessageType = "activity"

	// TypeInstructions is system context - the instructions that shape the run.
	// Always ordered ahead of everything else.
	TypeInstructions MessageType = "instructions"
)

// Message is one entry in the conversation.
type Message struct {
	Type MessageType `json:"type"`
	Text string      `json:"text"`

	// Activity is the tool call this message carries, on a TypeActivity
	// message. Nil on every other type.
	Activity *Activity `json:"activity,omitempty"`
}
