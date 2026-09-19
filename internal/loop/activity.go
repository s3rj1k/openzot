package loop

import "encoding/json"

// A tool call and its result, as first-class fields rather than a bag of keys.
//
// The engine used to carry these in a `map[string]any` under a `meta.activity`
// key, inherited from a hosted API whose messages are a generic envelope: one
// shape that has to carry attachments, ratings, triggers and a dozen other
// things zot has no concept of. zot has exactly one thing to put there - a tool
// call - and a map buys nothing for it: every read is a type assertion that can
// fail silently, a misspelt key is a compile-time success and a runtime no-op,
// and nothing tells you which keys are expected.
//
// So it is a struct. A missing field is a compile error, the shape is the
// documentation, and the pairing and rendering code stops guessing.

// ActivityKind is which half of a tool call a message carries.
type ActivityKind string

const (
	// ActivityRequest is the model asking for a tool to be run.
	ActivityRequest ActivityKind = "request"

	// ActivityResponse is what the tool returned.
	ActivityResponse ActivityKind = "response"

	// ActivityTrigger is an instruction to act now, carrying no call. Retained
	// because a conversation loaded from elsewhere can hold one; the engine
	// never produces them.
	ActivityTrigger ActivityKind = "trigger"
)

// Activity is one half of a tool-call pair.
type Activity struct {
	// Kind is which half this is.
	Kind ActivityKind `json:"kind"`

	// ID is the provider's call identifier, which is what joins the two halves.
	ID string `json:"id,omitempty"`

	// Name is the tool being called.
	Name string `json:"name,omitempty"`

	// Arguments is the call's arguments as the provider sent them - a JSON
	// string, kept verbatim rather than decoded, because it is replayed to the
	// provider exactly as received and re-encoding could change it.
	Arguments string `json:"arguments,omitempty"`

	// Result is what the tool returned, on a response. Whatever a handler
	// produced, so it is rendered as JSON when it reaches the model.
	Result any `json:"result,omitempty"`

	// Failure explains why a call could not be run. Set instead of Result.
	Failure string `json:"failure,omitempty"`
}

// IsPair reports whether two activities are the two halves of one call.
//
// Identity wins when both sides have one: providers issue a call id precisely so
// a result can name its call, and two identical calls in the same turn are only
// distinguishable that way. Name and arguments are the fallback, for histories
// reconstructed from somewhere that did not keep ids.
func (a *Activity) IsPair(other *Activity) bool {
	if a == nil || other == nil {
		return false
	}

	// a pair is one of each
	if a.Kind == other.Kind {
		return false
	}

	for _, kind := range []ActivityKind{a.Kind, other.Kind} {
		if kind != ActivityRequest && kind != ActivityResponse {
			return false
		}
	}

	if a.ID != "" && other.ID != "" {
		return a.ID == other.ID
	}

	return a.Name == other.Name && a.Arguments == other.Arguments
}

// ResultText renders what the model is shown for a response.
//
// A failure is presented as a JSON object rather than bare prose so the model
// sees a tool result of the shape it expects, with the error inside it.
func (a *Activity) ResultText() string {
	if a == nil {
		return ""
	}

	value := a.output()

	if value == nil {
		return ""
	}

	if text, ok := value.(string); ok {
		return text
	}

	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}

	return string(encoded)
}
