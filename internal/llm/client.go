package llm

import (
	"context"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// Client is a configured connection to a model.
type Client struct {
	config Config
	model  fantasy.LanguageModel
}

// New validates the configuration and connects to the endpoint it names.
func New(config Config) (*Client, error) {
	resolved, err := config.Resolve()
	if err != nil {
		return nil, err
	}

	options := []openaicompat.Option{
		openaicompat.WithBaseURL(resolved.BaseURL),
		openaicompat.WithAPIKey(resolved.APIKey),
		openaicompat.WithHTTPClient(newHTTPClient()),
		openaicompat.WithSDKOptions(option.WithMiddleware(wire(resolved))),
		openaicompat.WithUserAgent("zot"),
		openaicompat.WithLanguageModelOptions(openai.WithLanguageModelStreamUsageFunc(streamUsage)),
	}

	provider, err := openaicompat.New(options...)
	if err != nil {
		return nil, err
	}

	model, err := provider.LanguageModel(context.Background(), resolved.Model)
	if err != nil {
		return nil, err
	}

	return &Client{config: resolved, model: model}, nil
}

// Config returns the resolved configuration.
func (c *Client) Config() Config {
	return c.config
}

// Stream runs one model call. A failure to start it arrives as an error part,
// the same way a failure mid-stream does, so a caller has one place to look.
func (c *Client) Stream(ctx context.Context, call fantasy.Call) fantasy.StreamResponse {
	return func(yield func(fantasy.StreamPart) bool) {
		stream, err := c.model.Stream(ctx, call)
		if err != nil {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: err})

			return
		}

		for part := range stream {
			if !yield(part) {
				return
			}
		}
	}
}

// streamUsage reads a chunk's token counts. fantasy ignores a usage block that
// carries no total, and a server is free to leave the total out.
func streamUsage(
	chunk openaisdk.ChatCompletionChunk,
	extra map[string]any,
	metadata fantasy.ProviderMetadata,
) (fantasy.Usage, fantasy.ProviderMetadata) {
	if chunk.Usage.TotalTokens == 0 {
		chunk.Usage.TotalTokens = chunk.Usage.PromptTokens + chunk.Usage.CompletionTokens
	}

	return openai.DefaultStreamUsageFunc(chunk, extra, metadata)
}
