package loop_test

import (
	"strings"

	"charm.land/fantasy"

	"github.com/openzot/openzot/internal/conversation"
)

// activity builds one half of a tool-call pair.
func activity(kind conversation.ActivityKind, id, name, arguments string, result any) conversation.Message {
	entry := &conversation.Activity{Kind: kind, ID: id, Name: name, Arguments: arguments}

	if kind == conversation.ActivityResponse {
		entry.Result = result
	}

	return conversation.Message{Type: conversation.TypeActivity, Text: entry.ResultText(), Activity: entry}
}

// request builds a tool-call message.
func request(id, name, arguments string) conversation.Message {
	return conversation.Message{
		Type:     conversation.TypeActivity,
		Activity: &conversation.Activity{Kind: conversation.ActivityRequest, ID: id, Name: name, Arguments: arguments},
	}
}

// response builds the result half of a tool call.
func response(id, name, arguments, result string) conversation.Message {
	return conversation.Message{
		Type: conversation.TypeActivity,
		Text: result,
		Activity: &conversation.Activity{
			Kind:      conversation.ActivityResponse,
			ID:        id,
			Name:      name,
			Arguments: arguments,
			Result:    result,
		},
	}
}

// toolCallOf returns the call an assistant message carries, if it carries one.
func toolCallOf(message fantasy.Message) (fantasy.ToolCallPart, bool) {
	for _, part := range message.Content {
		if call, ok := part.(fantasy.ToolCallPart); ok {
			return call, true
		}
	}

	return fantasy.ToolCallPart{}, false
}

// textOf joins the text parts of a message.
func textOf(message fantasy.Message) string {
	var text string

	var textSb58 strings.Builder

	for _, part := range message.Content {
		if piece, ok := part.(fantasy.TextPart); ok {
			textSb58.WriteString(piece.Text)
		}
	}

	text += textSb58.String()

	return text
}
