package provider

import (
	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
)

// The rules that shape a request to what OpenAI-compatible servers accept.
//
// Fantasy's provider builds the request and offers a hook at each point where zot
// needs it to come out differently. Each hook here wraps fantasy's own compat
// function rather than replacing it, so what that does - reasoning effort, extra
// body, tool-result media - still happens.

// prepareCall is the request parameters, once fantasy has set them.
//
// The token limit goes out as max_tokens, the field every OpenAI-compatible
// server reads. Fantasy sends max_completion_tokens instead for any model whose
// name looks like a reasoning model, which a compat server behind a gateway alias
// such as "gpt-5" would never see.
func prepareCall(
	model fantasy.LanguageModel,
	params *openaisdk.ChatCompletionNewParams,
	call fantasy.Call, //nolint:gocritic // hugeParam: the prepare-call callback type is fantasy's
) ([]fantasy.CallWarning, error) {
	warnings, err := openaicompat.PrepareCallFunc(model, params, call)
	if err != nil {
		return nil, err
	}

	if params.MaxCompletionTokens.Valid() {
		params.MaxTokens = params.MaxCompletionTokens
		params.MaxCompletionTokens = param.Opt[int64]{}
	}

	return warnings, nil
}

// textParts is one text part holding text, or none when there is no text.
func textParts(text param.Opt[string]) []openaisdk.ChatCompletionContentPartTextParam {
	if !text.Valid() || text.Value == "" {
		return []openaisdk.ChatCompletionContentPartTextParam{}
	}

	return []openaisdk.ChatCompletionContentPartTextParam{{Text: text.Value}}
}

// asParts turns a message's content into an array of parts.
func asParts(message *openaisdk.ChatCompletionMessageParamUnion) {
	switch {
	case message.OfSystem != nil:
		content := &message.OfSystem.Content

		if content.OfString.Valid() || len(content.OfArrayOfContentParts) == 0 {
			content.OfArrayOfContentParts = textParts(content.OfString)
			content.OfString = param.Opt[string]{}
		}

	case message.OfDeveloper != nil:
		content := &message.OfDeveloper.Content

		if content.OfString.Valid() || len(content.OfArrayOfContentParts) == 0 {
			content.OfArrayOfContentParts = textParts(content.OfString)
			content.OfString = param.Opt[string]{}
		}

	case message.OfTool != nil:
		content := &message.OfTool.Content

		if content.OfString.Valid() || len(content.OfArrayOfContentParts) == 0 {
			content.OfArrayOfContentParts = textParts(content.OfString)
			content.OfString = param.Opt[string]{}
		}

	case message.OfUser != nil:
		content := &message.OfUser.Content

		if content.OfString.Valid() || len(content.OfArrayOfContentParts) == 0 {
			parts := make([]openaisdk.ChatCompletionContentPartUnionParam, 0, 1)

			for _, part := range textParts(content.OfString) {
				parts = append(parts, openaisdk.ChatCompletionContentPartUnionParam{OfText: &part})
			}

			content.OfArrayOfContentParts = parts
			content.OfString = param.Opt[string]{}
		}

	case message.OfAssistant != nil:
		content := &message.OfAssistant.Content

		if content.OfString.Valid() || len(content.OfArrayOfContentParts) == 0 {
			parts := make([]openaisdk.ChatCompletionAssistantMessageParamContentArrayOfContentPartUnion, 0, 1)

			for _, part := range textParts(content.OfString) {
				parts = append(parts, openaisdk.ChatCompletionAssistantMessageParamContentArrayOfContentPartUnion{OfText: &part})
			}

			content.OfArrayOfContentParts = parts
			content.OfString = param.Opt[string]{}
		}
	}
}

// contentArrayPrompt is the conversation as messages with every content sent as an
// array of parts, and an empty one as []. What some servers' chat templates want,
// llama.cpp's among them, and reject a bare string without.
func contentArrayPrompt(prompt fantasy.Prompt, provider, model string) ([]openaisdk.ChatCompletionMessageParamUnion, []fantasy.CallWarning) {
	messages, warnings := openaicompat.ToPromptFunc(prompt, provider, model)

	for i := range messages {
		asParts(&messages[i])
	}

	return messages, warnings
}

// languageModelOptions are the hooks a client passes fantasy's provider.
func languageModelOptions(config ClientConfig) []openai.LanguageModelOption {
	options := []openai.LanguageModelOption{
		openai.WithLanguageModelPrepareCallFunc(prepareCall),
		openai.WithLanguageModelStreamUsageFunc(streamUsage),
	}

	if config.ContentArray {
		options = append(options, openai.WithLanguageModelToPromptFunc(contentArrayPrompt))
	}

	return options
}
