package loop

import (
	"charm.land/fantasy"

	"github.com/openzot/openzot/internal/thread"
)

// Conversions between the loop's message shape and the ones the subsystems use.
//
// They are kept in one file rather than scattered because getting a conversion
// wrong is silent: the thread heuristics need type and meta to spot a loop, and
// a conversion that drops meta simply stops detecting anything.

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
	MessageType MessageType
	Activity    *Activity

	// Args are a tool call's decoded arguments; Result is what it returned.
	Args   map[string]any
	Result any

	// InputTokens and OutputTokens carry the run's cumulative provider-reported
	// token usage on an EventUsage.
	InputTokens  int
	OutputTokens int

	// Failure is the provider error behind an EventRetry, so a consumer can
	// persist the failing exchange the moment it happens rather than waiting
	// for the run to end - which a kill would never reach.
	Failure error
}

func toThreadMessages(messages []Message) []thread.Message {
	converted := make([]thread.Message, 0, len(messages))

	for _, message := range messages {
		entry := thread.Message{"type": string(message.Type), "text": message.Text}

		// the heuristics compare the platform's map shape, because their corpus
		// is JSON captured from the TypeScript implementation
		if meta := message.Activity.threadMeta(); meta != nil {
			entry["meta"] = meta
		}

		converted = append(converted, entry)
	}

	return converted
}

func fromThreadMessages(messages []thread.Message) []Message {
	converted := make([]Message, 0, len(messages))

	for _, message := range messages {
		entry := Message{Type: MessageType(message.Type()), Text: message.Text()}

		if meta, ok := message.Meta(); ok {
			entry.Activity = activityFromMeta(meta)
		}

		converted = append(converted, entry)
	}

	return converted
}

// toPrompt renders the conversation into the prompt a model call carries.
//
// Activity messages are the interesting case: a request half becomes an
// assistant turn carrying a tool call, and a response half becomes a tool
// message referencing the same id. Providers validate that pairing, so a
// response whose request was trimmed away is dropped rather than sent.
func toPrompt(messages []Message) fantasy.Prompt {
	var (
		prompt  fantasy.Prompt
		pending = map[string]bool{}
	)

	// repair the history before rendering it: a provider rejects the whole
	// request rather than the invalid part, so anything left unpaired here ends
	// an otherwise healthy run with an opaque 400
	for _, message := range Organize(messages) {
		switch message.Type {
		case TypeActivity:
			activity := message.Activity

			if activity == nil {
				continue
			}

			switch activity.Kind {
			case ActivityRequest:
				pending[activity.ID] = true

				prompt = append(prompt, fantasy.Message{
					Role: fantasy.MessageRoleAssistant,
					Content: []fantasy.MessagePart{fantasy.ToolCallPart{
						ToolCallID: activity.ID,
						ToolName:   activity.Name,
						Input:      activity.Arguments,
					}},
				})

			case ActivityResponse:
				// a result whose call was trimmed away would be rejected
				if !pending[activity.ID] {
					continue
				}

				delete(pending, activity.ID)

				prompt = append(prompt, fantasy.Message{
					Role: fantasy.MessageRoleTool,
					Content: []fantasy.MessagePart{fantasy.ToolResultPart{
						ToolCallID: activity.ID,
						Output:     fantasy.ToolResultOutputContentText{Text: activity.ResultText()},
					}},
				})
			}

		case TypeBot:
			prompt = append(prompt, fantasy.Message{
				Role:    fantasy.MessageRoleAssistant,
				Content: []fantasy.MessagePart{fantasy.TextPart{Text: message.Text}},
			})

		case TypeReasoning:
			// the reasoning channel is not replayed: providers reject their own
			// reasoning content on the way back in, and it is the model's
			// scratchpad rather than conversation

		case TypeInstructions:
			prompt = append(prompt, fantasy.NewSystemMessage(message.Text))

		default:
			prompt = append(prompt, fantasy.NewUserMessage(message.Text))
		}
	}

	// an assistant turn requesting a call that never got a result leaves the
	// conversation invalid; drop the dangling halves
	if len(pending) > 0 {
		prompt = dropDangling(prompt, pending)
	}

	return prompt
}

// dropDangling removes assistant tool-call turns whose results are missing.
func dropDangling(prompt fantasy.Prompt, pending map[string]bool) fantasy.Prompt {
	kept := make(fantasy.Prompt, 0, len(prompt))

	for _, message := range prompt {
		if call, ok := singleToolCall(message); ok && pending[call.ToolCallID] {
			continue
		}

		kept = append(kept, message)
	}

	return kept
}

// singleToolCall returns the call an assistant message consists of, if it is one.
func singleToolCall(message fantasy.Message) (fantasy.ToolCallPart, bool) {
	if message.Role != fantasy.MessageRoleAssistant || len(message.Content) != 1 {
		return fantasy.ToolCallPart{}, false
	}

	call, ok := message.Content[0].(fantasy.ToolCallPart)

	return call, ok
}
