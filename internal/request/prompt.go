// Package request turns a conversation into what a model call carries. It pairs each tool call with its result, drops
// what a provider would reject, and makes sure the request starts and ends on a user turn.
package request

import (
	"charm.land/fantasy"

	"github.com/openzot/openzot/internal/conversation"
)

// singleToolCall returns the call an assistant message consists of, if it is one.
func singleToolCall(message fantasy.Message) (fantasy.ToolCallPart, bool) {
	if message.Role != fantasy.MessageRoleAssistant || len(message.Content) != 1 {
		return fantasy.ToolCallPart{}, false
	}

	call, ok := message.Content[0].(fantasy.ToolCallPart)

	return call, ok
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

// ToPrompt renders the conversation into the prompt a model call carries. A request half becomes an assistant
// turn with a tool call and a response half a tool message with the same id, and a result whose call was
// trimmed away is dropped, since providers validate the pairing.
func ToPrompt(messages []conversation.Message) fantasy.Prompt {
	var (
		prompt  fantasy.Prompt
		pending = map[string]bool{}
	)

	// Repair the history before rendering it. A provider rejects the whole request rather than the
	// invalid part, so anything left unpaired here ends a healthy run with an opaque 400.
	for _, message := range conversation.Organize(messages) {
		switch message.Type {
		case conversation.TypeActivity:
			activity := message.Activity

			if activity == nil {
				continue
			}

			switch activity.Kind {
			case conversation.ActivityRequest:
				pending[activity.ID] = true

				prompt = append(prompt, fantasy.Message{
					Role: fantasy.MessageRoleAssistant,
					Content: []fantasy.MessagePart{fantasy.ToolCallPart{
						ToolCallID: activity.ID,
						ToolName:   activity.Name,
						Input:      activity.Arguments,
					}},
				})

			case conversation.ActivityResponse:
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

			default:
				// a trigger carries no call, so there is nothing to send
			}

		case conversation.TypeBot:
			prompt = append(prompt, fantasy.Message{
				Role:    fantasy.MessageRoleAssistant,
				Content: []fantasy.MessagePart{fantasy.TextPart{Text: message.Text}},
			})

		case conversation.TypeReasoning:
			// The reasoning channel is not replayed. Providers reject their own reasoning on the way back in,
			// and it is the model's scratchpad rather than conversation.

		case conversation.TypeInstructions:
			prompt = append(prompt, fantasy.NewSystemMessage(message.Text))

		default:
			prompt = append(prompt, fantasy.NewUserMessage(message.Text))
		}
	}

	// an assistant turn requesting a call that never got a result leaves the
	// conversation invalid. Drop the dangling halves
	if len(pending) > 0 {
		prompt = dropDangling(prompt, pending)
	}

	return prompt
}

// Kickoff stands in for the opening user message once forgetting has dropped it. The goal lives in the instructions, so
// this only has to exist and point there.
const Kickoff = "Continue working on your task as stated in the instructions."

// Build is the prompt a model call is sent for the messages the window still holds. It is ToPrompt, made acceptable to a
// strict provider. Forgetting takes the oldest first, which is the opening user message, and a conversation with no user
// turn is rejected, so one is restored. The goal itself is safe in the instructions. The prompt also has to end on a user
// or tool turn, since fantasy will not start a step from a conversation ending on the model's own words. The engine never
// leaves one, but a handed-in conversation might, and one more line costs less than a run that cannot start.
func Build(messages []conversation.Message) fantasy.Prompt {
	chat := ToPrompt(messages)

	hasUser := false

	for _, message := range chat {
		if message.Role == fantasy.MessageRoleUser {
			hasUser = true

			break
		}
	}

	if !hasUser {
		chat = append(fantasy.Prompt{fantasy.NewUserMessage(Kickoff)}, chat...)
	}

	if last := chat[len(chat)-1]; last.Role != fantasy.MessageRoleUser && last.Role != fantasy.MessageRoleTool {
		chat = append(chat, fantasy.NewUserMessage(Kickoff))
	}

	return chat
}
