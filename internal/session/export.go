package session

import (
	"encoding/json"
	"fmt"
	"time"
)

// Trajectory is a session rendered for consumption outside zot: the
// conversation in the chat shape the rest of the ecosystem speaks - system,
// user, assistant with tool_calls, tool - plus the facts about the run that
// make one trajectory comparable to the next.
//
// The shape is the OpenAI messages convention because it is what training and
// evaluation tooling already loads: a file of these is a dataset, not a format
// somebody has to write a parser for. Where zot knows more than that shape can
// hold - which of its own message types a turn was, the model's reasoning - it is
// carried in additive fields a loader that does not know them simply drops.
type Trajectory struct {
	// ID is the session this trajectory was exported from.
	ID string `json:"id"`

	Task     string `json:"task"`
	Model    string `json:"model"`
	Provider string `json:"provider"`
	Driver   string `json:"driver"`
	Workdir  string `json:"workdir,omitempty"`

	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended"`

	// Outcome is how the run ended; nil when it did not.
	Outcome *Result `json:"outcome,omitempty"`

	// Complete is whether an outcome was recorded, and Truncated whether the
	// log ended mid-record - a run that was killed rather than one that stopped.
	Complete  bool `json:"complete"`
	Truncated bool `json:"truncated,omitempty"`

	// Messages is the conversation as it stood at the end, every turn that
	// happened - the conversation is only ever appended to.
	Messages []ChatMessage `json:"messages"`

	// Events counts what happened by kind - iterations, nudges, retries - for
	// filtering without walking the conversation.
	Events map[string]int `json:"events,omitempty"`
}

// ChatMessage is one turn in the exported conversation.
type ChatMessage struct {
	// Role is the chat-convention role: system, user, assistant or tool.
	Role string `json:"role"`

	// Type is zot's own message type - instructions, user, bot, activity - which
	// says more than the role does.
	Type string `json:"type"`

	// Content is the turn's text.
	Content string `json:"content"`

	// Reasoning is the model's scratchpad for the turn, when the provider
	// surfaced it.
	Reasoning string `json:"reasoning,omitempty"`

	// ToolCalls are the calls an assistant turn made.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`

	// ToolCallID and Name identify which call a tool turn answers.
	ToolCallID string `json:"tool_call_id,omitempty"`
	Name       string `json:"name,omitempty"`
}

// ToolCall is one call in an assistant turn, in the OpenAI shape.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall names the tool and carries its arguments verbatim.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Export renders a session as a trajectory.
func Export(s *Session) (*Trajectory, error) {
	if s == nil {
		return nil, fmt.Errorf("session: nothing to export")
	}

	trajectory := &Trajectory{
		ID:        s.Meta.ID,
		Task:      s.Meta.Task,
		Model:     s.Meta.Model,
		Provider:  s.Meta.Provider,
		Driver:    s.Meta.Driver,
		Workdir:   s.Meta.Workdir,
		Started:   s.Started,
		Ended:     s.Ended,
		Outcome:   s.Result,
		Complete:  s.Complete(),
		Truncated: s.Truncated,
		Events:    map[string]int{},
	}

	for _, event := range s.Events {
		trajectory.Events[event.Kind]++
	}

	trajectory.Messages = convert(s.Messages)

	return trajectory, nil
}

// convert renders one conversation.
//
// The turns zot records are finer than the chat shape: a turn's reasoning, its
// text and each of its tool calls are separate records, and the chat shape
// wants them as one assistant message. Consecutive assistant-side records are
// folded into one message until something else - a tool result, a user turn -
// closes it.
func convert(messages []Message) []ChatMessage {
	var (
		out     []ChatMessage
		pending *ChatMessage
	)

	flush := func() {
		if pending != nil {
			out = append(out, *pending)
			pending = nil
		}
	}

	assistant := func(typ string) *ChatMessage {
		if pending == nil {
			pending = &ChatMessage{Role: "assistant", Type: typ, Content: ""}
		}

		return pending
	}

	for _, message := range messages {
		switch message.Type {
		case "instructions":
			flush()
			out = append(out, ChatMessage{Role: "system", Type: message.Type, Content: message.Text})

		case "user":
			flush()
			out = append(out, ChatMessage{Role: "user", Type: message.Type, Content: message.Text})

		case "reasoning":
			turn := assistant("bot")

			if turn.Reasoning != "" {
				turn.Reasoning += "\n\n"
			}

			turn.Reasoning += message.Text

		case "bot":
			turn := assistant("bot")

			// a second answer in one turn does not happen; if a log holds one,
			// it is a new turn
			if turn.Content != "" || len(turn.ToolCalls) > 0 {
				flush()
				turn = assistant("bot")
			}

			turn.Content = message.Text

		case "activity":
			activity := message.Activity
			if activity == nil {
				continue
			}

			switch activity.Kind {
			case "request":
				turn := assistant("bot")
				turn.ToolCalls = append(turn.ToolCalls, ToolCall{
					ID:       activity.ID,
					Type:     "function",
					Function: FunctionCall{Name: activity.Name, Arguments: activity.Arguments},
				})

			case "response":
				flush()
				out = append(out, ChatMessage{
					Role:       "tool",
					Type:       message.Type,
					Content:    toolContent(activity),
					ToolCallID: activity.ID,
					Name:       activity.Name,
				})

			default:
				// a trigger is an instruction to act, which is a user turn in
				// every chat convention
				flush()
				out = append(out, ChatMessage{Role: "user", Type: message.Type, Content: message.Text})
			}

		default:
			// a type this build does not know is still a turn; a user turn is
			// the one that loses nothing
			flush()
			out = append(out, ChatMessage{Role: "user", Type: message.Type, Content: message.Text})
		}
	}

	flush()

	return out
}

// toolContent renders what a tool returned the way the model read it: a string
// as itself, anything else as JSON, a failure as its text.
func toolContent(activity *Activity) string {
	if activity.Failure != "" {
		return activity.Failure
	}

	switch result := activity.Result.(type) {
	case nil:
		return ""
	case string:
		return result
	default:
		encoded, err := json.Marshal(result)
		if err != nil {
			return fmt.Sprint(result)
		}

		return string(encoded)
	}
}
