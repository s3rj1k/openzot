package llm

import (
	"testing"

	"charm.land/fantasy"
)

func TestDecodeArguments(t *testing.T) {
	for _, empty := range []string{"", "   "} {
		arguments, err := DecodeArguments(fantasy.ToolCallContent{ToolName: "shell", Input: empty})
		if err != nil || len(arguments) != 0 {
			t.Errorf("%q decoded to %v, %v, want an empty object", empty, arguments, err)
		}
	}

	arguments, err := DecodeArguments(fantasy.ToolCallContent{ToolName: "shell", Input: `{"cmd":"ls"}`})
	if err != nil || arguments["cmd"] != "ls" {
		t.Errorf("decoded %v, %v", arguments, err)
	}

	if _, err := DecodeArguments(fantasy.ToolCallContent{ToolName: "shell", Input: `{"cmd":`}); err == nil {
		t.Error("truncated JSON must not decode")
	}
}
