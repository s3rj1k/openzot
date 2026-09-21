package testutils

import (
	"fmt"
	"strings"

	"charm.land/fantasy"

	"github.com/s3rj1k/agent/internal/conversation"
)

// Activity builds one half of a tool-call pair. Only a response carries the result.
func Activity(kind conversation.ActivityKind, id, name, arguments string, result any) conversation.Message {
	entry := &conversation.Activity{Kind: kind, ID: id, Name: name, Arguments: arguments}

	if kind == conversation.ActivityResponse {
		entry.Result = result
	}

	return conversation.Message{Type: conversation.TypeActivity, Text: entry.ResultText(), Activity: entry}
}

// Request builds a tool-call message.
func Request(id, name, arguments string) conversation.Message {
	return conversation.Message{
		Type:     conversation.TypeActivity,
		Activity: &conversation.Activity{Kind: conversation.ActivityRequest, ID: id, Name: name, Arguments: arguments},
	}
}

// Response builds the result half of a tool call.
func Response(id, name, arguments, result string) conversation.Message {
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

// Trigger builds the message that tells the model to act now.
func Trigger(name string) conversation.Message {
	return conversation.Message{
		Type:     conversation.TypeActivity,
		Activity: &conversation.Activity{Kind: conversation.ActivityTrigger, Name: name},
	}
}

// ToolCallOf returns the call an assistant message carries, if it carries one.
func ToolCallOf(message fantasy.Message) (fantasy.ToolCallPart, bool) {
	for _, part := range message.Content {
		if call, ok := part.(fantasy.ToolCallPart); ok {
			return call, true
		}
	}

	return fantasy.ToolCallPart{}, false
}

// TextOf joins the text parts of a message.
func TextOf(message fantasy.Message) string {
	var text strings.Builder

	for _, part := range message.Content {
		if piece, ok := part.(fantasy.TextPart); ok {
			text.WriteString(piece.Text)
		}
	}

	return text.String()
}

// Task is one entry of a tasks tool call.
func Task(title, status string) map[string]any {
	return map[string]any{"title": title, "status": status}
}

// TaskCall is the arguments of a tasks tool call that carries the given tasks.
func TaskCall(tasks ...map[string]any) map[string]any {
	list := make([]any, 0, len(tasks))

	for _, task := range tasks {
		list = append(list, task)
	}

	return map[string]any{"tasks": list}
}

// TasksArgs is the arguments of a tasks call as the model sends them, each task a title, a status and an optional note.
func TasksArgs(tasks ...[3]string) map[string]any {
	list := make([]any, 0, len(tasks))

	for _, task := range tasks {
		entry := map[string]any{"title": task[0], "status": task[1]}

		if task[2] != "" {
			entry["note"] = task[2]
		}

		list = append(list, entry)
	}

	return map[string]any{"tasks": list}
}

// PlanArgs is a tasks call as the model sends one, two tasks with one done.
const PlanArgs = `{"tasks":[{"title":"read the code","status":"done"},{"title":"fix it","status":"in_progress"}]}`

// PlanCall is the model calling its plan tool and being answered.
func PlanCall(id, args, answer string) []conversation.Message {
	return []conversation.Message{
		Activity(conversation.ActivityRequest, id, "tasks", args, nil),
		Activity(conversation.ActivityResponse, id, "tasks", args, answer),
	}
}

// History is a kickoff and a plan, then n turns of tool work each carrying the filler as the result.
func History(n int, filler string) []conversation.Message {
	messages := make([]conversation.Message, 0, 3+2*n)
	messages = append(messages, conversation.Message{Type: conversation.TypeUser, Text: "kickoff"})
	messages = append(messages, PlanCall("plan", PlanArgs, "the plan")...)

	for i := range n {
		id := fmt.Sprintf("c%d", i)

		messages = append(messages,
			Activity(conversation.ActivityRequest, id, "read", `{"path":"x"}`, nil),
			Activity(conversation.ActivityResponse, id, "read", `{"path":"x"}`, filler),
		)
	}

	return messages
}

// Starts is where each turn of a History begins, the plan being turn one and every pair after it another, with the turn
// about to be asked for last.
func Starts(messages []conversation.Message) []int {
	out := []int{0, 1}

	for i := 3; i < len(messages); i += 2 {
		out = append(out, i)
	}

	return append(out, len(messages))
}
