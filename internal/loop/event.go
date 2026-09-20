package loop

import "github.com/openzot/openzot/internal/conversation"

// EventKind identifies what happened.
type EventKind string

const (
	EventIteration      EventKind = "iteration"
	EventToken          EventKind = "token"
	EventReasoningToken EventKind = "reasoningToken"
	EventMessage        EventKind = "message"
	EventToolCallStart  EventKind = "toolCallStart"
	EventToolCallEnd    EventKind = "toolCallEnd"
	EventToolCallError  EventKind = "toolCallError"
	EventRetry          EventKind = "retry"
	EventRunaway        EventKind = "runaway"
	EventNotice         EventKind = "notice"
	EventUsage          EventKind = "usage"
)

// Event is emitted as a run progresses.
type Event struct {
	Kind      EventKind
	Text      string
	Tool      string
	Iteration int

	// MessageType and Activity describe a completed message.
	MessageType conversation.MessageType
	Activity    *conversation.Activity

	// Args are a tool call's decoded arguments. Result is what it returned.
	Args   map[string]any
	Result any

	// InputTokens and OutputTokens carry the run's cumulative provider-reported
	// token usage on an EventUsage.
	InputTokens  int
	OutputTokens int

	/*
		Failure is the provider error behind an EventRetry, so a consumer can
		persist the failing exchange the moment it happens rather than waiting
		for the run to end - which a kill would never reach.
	*/
	Failure error
}
