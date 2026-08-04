package llm

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// AnthropicProvider — провайдер на официальном Anthropic SDK.
type AnthropicProvider struct {
	client    anthropic.Client
	model     string
	maxTokens int64
}

// NewAnthropicProvider создаёт провайдера. Если apiKey пуст,
// SDK возьмёт ключ из ANTHROPIC_API_KEY.
func NewAnthropicProvider(apiKey, model string, maxTokens int64) *AnthropicProvider {
	var opts []option.RequestOption
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	return &AnthropicProvider{
		client:    anthropic.NewClient(opts...),
		model:     model,
		maxTokens: maxTokens,
	}
}

func (p *AnthropicProvider) Stream(ctx context.Context, system string, msgs []Message, onDelta func(string)) (string, error) {
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(p.model),
		MaxTokens: p.maxTokens,
	}
	if system != "" {
		params.System = []anthropic.TextBlockParam{{Text: system}}
	}
	for _, m := range msgs {
		block := anthropic.NewTextBlock(m.Content)
		switch m.Role {
		case RoleAssistant:
			params.Messages = append(params.Messages, anthropic.NewAssistantMessage(block))
		default:
			params.Messages = append(params.Messages, anthropic.NewUserMessage(block))
		}
	}

	stream := p.client.Messages.NewStreaming(ctx, params)
	var sb strings.Builder
	for stream.Next() {
		event := stream.Current()
		switch e := event.AsAny().(type) {
		case anthropic.ContentBlockDeltaEvent:
			switch d := e.Delta.AsAny().(type) {
			case anthropic.TextDelta:
				sb.WriteString(d.Text)
				if onDelta != nil {
					onDelta(d.Text)
				}
			}
		}
	}
	if err := stream.Err(); err != nil {
		return sb.String(), fmt.Errorf("anthropic stream: %w", err)
	}
	return sb.String(), nil
}
