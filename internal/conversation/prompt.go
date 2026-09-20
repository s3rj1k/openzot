package conversation

import "charm.land/fantasy"

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

// ToPrompt renders the conversation into the prompt a model call carries.
//
// Activity messages are the interesting case. A request half becomes an
// assistant turn carrying a tool call, and a response half becomes a tool
// message referencing the same id. Providers validate that pairing, so a
// response whose request was trimmed away is dropped rather than sent.
func ToPrompt(messages []Message) fantasy.Prompt {
	var (
		prompt  fantasy.Prompt
		pending = map[string]bool{}
	)

	// Repair the history before rendering it. A provider rejects the whole request rather than the
	// invalid part, so anything left unpaired here ends a healthy run with an opaque 400.
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

			default:
				// a trigger carries no call, so there is nothing to send
			}

		case TypeBot:
			prompt = append(prompt, fantasy.Message{
				Role:    fantasy.MessageRoleAssistant,
				Content: []fantasy.MessagePart{fantasy.TextPart{Text: message.Text}},
			})

		case TypeReasoning:
			// The reasoning channel is not replayed. Providers reject their own reasoning on the way back in,
			// and it is the model's scratchpad rather than conversation.

		case TypeInstructions:
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
