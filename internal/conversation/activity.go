package conversation

import "encoding/json"

// A tool call and its result, as typed fields rather than a map of keys.
// A missing field is a compile error, and the shape is its own documentation.

// ActivityKind is which half of a tool call a message carries.
type ActivityKind string

const (
	// ActivityRequest is the model asking for a tool to be run.
	ActivityRequest ActivityKind = "request"

	// ActivityResponse is what the tool returned.
	ActivityResponse ActivityKind = "response"

	// ActivityTrigger is an instruction to act now, carrying no call. Kept
	// because a conversation loaded from elsewhere can hold one. The engine
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

	// The call's arguments as the provider sent them, a JSON string kept verbatim, since it is replayed
	// exactly as received and re-encoding could change it.
	Arguments string `json:"arguments,omitempty"`

	// Result is what the tool returned, on a response. Whatever a handler
	// produced, so it is rendered as JSON when it reaches the model.
	Result any `json:"result,omitempty"`

	// Failure explains why a call could not be run. Set instead of Result.
	Failure string `json:"failure,omitempty"`
}

// IsPair reports whether two activities are the two halves of one call. The call id decides when both
// sides have one. Name and arguments are the fallback for histories that did not keep ids.
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

// Output is what a response reports back, for comparison. The failure if there
// was one, otherwise the result.
func (a *Activity) Output() any {
	if a.Failure != "" {
		return map[string]any{"error": a.Failure}
	}

	return a.Result
}

// ResultText renders what the model is shown for a response. A failure is a JSON object, not bare prose,
// so the model sees a tool result of the shape it expects.
func (a *Activity) ResultText() string {
	if a == nil {
		return ""
	}

	value := a.Output()

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
