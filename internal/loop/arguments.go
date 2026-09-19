package loop

import (
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/fantasy"
)

// decodeArguments parses a tool call's JSON arguments.
//
// An empty string decodes to an empty object: a model calling a no-argument tool
// often sends "" rather than "{}", and rejecting that would fail the call for no
// reason.
func decodeArguments(call fantasy.ToolCallContent) (map[string]any, error) {
	raw := strings.TrimSpace(call.Input)

	if raw == "" {
		return map[string]any{}, nil
	}

	var arguments map[string]any

	if err := json.Unmarshal([]byte(raw), &arguments); err != nil {
		return nil, fmt.Errorf("tool %q: arguments are not valid JSON: %w", call.ToolName, err)
	}

	return arguments, nil
}
