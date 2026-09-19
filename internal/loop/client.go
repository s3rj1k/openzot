package loop

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
	config ClientConfig
	model  fantasy.LanguageModel
}

// NewClient validates the configuration and connects to the endpoint it names.
func NewClient(config ClientConfig) (*Client, error) {
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

	return &Client{config: resolved, model: toolCallsModel{model}}, nil
}

// Config returns the resolved configuration.
func (c *Client) Config() ClientConfig {
	return c.config
}

// toolCallsModel makes a turn that asks for tools a tool turn, whatever the
// provider called its ending.
//
// fantasy runs the tools of a turn only when it finished as "tool_calls", but
// endpoints in the wild finish a turn that carries tool calls as "stop", or as
// something unrecognised. The calls are what the model asked for, and they are
// run; only a turn cut short - by length, a content filter or an error - has
// calls that cannot be trusted, and those stay as fantasy reports them.
type toolCallsModel struct {
	fantasy.LanguageModel
}

func (m toolCallsModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	stream, err := m.LanguageModel.Stream(ctx, call)
	if err != nil {
		return nil, err
	}

	return func(yield func(fantasy.StreamPart) bool) {
		asked := false

		for part := range stream {
			switch {
			case part.Type == fantasy.StreamPartTypeToolCall:
				asked = true
			case part.Type == fantasy.StreamPartTypeFinish && asked &&
				(part.FinishReason == fantasy.FinishReasonStop || part.FinishReason == fantasy.FinishReasonUnknown):
				part.FinishReason = fantasy.FinishReasonToolCalls
			}

			if !yield(part) {
				return
			}
		}
	}, nil
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
