// Package provider is zot's connection to a model. An OpenAI-compatible endpoint reached through fantasy, the
// classification of what goes wrong with it (retriable, rate limited, context too long), and the evidence a
// failed exchange leaves. It knows nothing of the conversation being had over it.
package provider

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

// sdkOptions keeps the SDK to the operator's credential. The SDK would default to OPENAI_API_KEY, ORG_ID and
// PROJECT_ID from the environment, so an empty key here overrides it, no Authorization header goes out, and the
// organization and project headers are removed. The key is the operator's, scoped to its endpoint, or there is none.
func sdkOptions(config ClientConfig) []option.RequestOption {
	return []option.RequestOption{
		option.WithAPIKey(config.APIKey),
		option.WithHeaderDel("OpenAI-Organization"),
		option.WithHeaderDel("OpenAI-Project"),
	}
}

// NewClient validates the configuration and connects to the endpoint it names.
func NewClient(ctx context.Context, config ClientConfig) (*Client, error) {
	resolved, err := config.Resolve()
	if err != nil {
		return nil, err
	}

	options := []openaicompat.Option{
		openaicompat.WithBaseURL(resolved.BaseURL),
		openaicompat.WithAPIKey(resolved.APIKey),
		openaicompat.WithHTTPClient(newHTTPClient()),
		openaicompat.WithSDKOptions(sdkOptions(resolved)...),
		openaicompat.WithUserAgent("zot"),
		openaicompat.WithLanguageModelOptions(languageModelOptions(resolved)...),
	}

	provider, err := openaicompat.New(options...)
	if err != nil {
		return nil, err
	}

	model, err := provider.LanguageModel(ctx, resolved.Model)
	if err != nil {
		return nil, err
	}

	return &Client{config: resolved, model: toolCallsModel{model}}, nil
}

// Config returns the resolved configuration.
func (c *Client) Config() ClientConfig {
	return c.config
}

// Model is the language model the client talks to, for an agent to run on.
func (c *Client) Model() fantasy.LanguageModel {
	return c.model
}

// toolCallsModel makes a turn that asks for tools a tool turn, whatever the provider called its ending. Fantasy runs
// tools only when a turn finished as "tool_calls", but endpoints finish such turns as "stop" or something else. Only a
// turn cut short by length, a content filter or an error has calls that cannot be trusted.
type toolCallsModel struct {
	fantasy.LanguageModel
}

func (m toolCallsModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) { //nolint:gocritic // hugeParam: fantasy.LanguageModel takes the call by value
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

// streamUsage reads a chunk's token counts. Fantasy ignores a usage block that
// carries no total, and a server is free to leave the total out.
func streamUsage(
	chunk openaisdk.ChatCompletionChunk, //nolint:gocritic // hugeParam: the stream usage callback type is fantasy's
	extra map[string]any,
	metadata fantasy.ProviderMetadata,
) (fantasy.Usage, fantasy.ProviderMetadata) {
	if chunk.Usage.TotalTokens == 0 {
		chunk.Usage.TotalTokens = chunk.Usage.PromptTokens + chunk.Usage.CompletionTokens
	}

	return openai.DefaultStreamUsageFunc(chunk, extra, metadata)
}
