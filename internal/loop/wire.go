package loop

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/openai/openai-go/v3/option"
)

// wire is the one place zot reshapes what the SDK is about to send.
//
// Three things, none of which the SDK can be asked for. The token limit goes
// out as max_tokens, the field every OpenAI-compatible server reads, not the
// max_completion_tokens the SDK prefers. With ContentArray every message's
// content becomes an array of parts, and an empty one []. And no credential
// the SDK picked up from the environment survives: the key is the operator's,
// or there is none.
func wire(config ClientConfig) option.Middleware {
	return func(request *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		if config.APIKey == "" {
			request.Header.Del("Authorization")
		}

		request.Header.Del("OpenAI-Organization")
		request.Header.Del("OpenAI-Project")

		if request.Body != nil {
			raw, err := io.ReadAll(request.Body)
			if err != nil {
				return nil, err
			}

			raw = reshape(raw, config.ContentArray)

			request.Body = io.NopCloser(bytes.NewReader(raw))
			request.ContentLength = int64(len(raw))
		}

		return next(request)
	}
}

// reshape rewrites a chat-completions request body, leaving anything it cannot
// parse exactly as it was.
func reshape(raw []byte, contentArray bool) []byte {
	var body map[string]any

	if json.Unmarshal(raw, &body) != nil {
		return raw
	}

	if limit, ok := body["max_completion_tokens"]; ok {
		delete(body, "max_completion_tokens")

		body["max_tokens"] = limit
	}

	if messages, ok := body["messages"].([]any); ok && contentArray {
		for _, entry := range messages {
			if message, ok := entry.(map[string]any); ok {
				message["content"] = asParts(message["content"])
			}
		}
	}

	reshaped, err := json.Marshal(body)
	if err != nil {
		return raw
	}

	return reshaped
}

// asParts renders message content as an array of parts.
func asParts(content any) any {
	switch value := content.(type) {
	case string:
		if value == "" {
			return []any{}
		}

		return []any{map[string]any{"type": "text", "text": value}}
	case nil:
		return []any{}
	default:
		return content
	}
}
