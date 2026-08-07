package llm

import (
	"context"
	"encoding/json"
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

// CallTool заставляет Claude вернуть типизированный результат через tool use.
func (p *AnthropicProvider) CallTool(ctx context.Context, system string, msgs []Message, tool Tool) (json.RawMessage, Usage, error) {
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

	inputSchema := anthropic.ToolInputSchemaParam{
		Properties: tool.Properties,
		Required:   tool.Required,
		ExtraFields: map[string]any{
			"additionalProperties": false,
		},
	}
	toolParam := anthropic.ToolUnionParamOfTool(inputSchema, tool.Name)
	toolParam.OfTool.Description = anthropic.String(tool.Description)
	toolParam.OfTool.Strict = anthropic.Bool(true)
	params.Tools = []anthropic.ToolUnionParam{toolParam}
	params.ToolChoice = anthropic.ToolChoiceParamOfTool(tool.Name)

	message, err := p.client.Messages.New(ctx, params)
	if err != nil {
		// ctx.Err() != nil означает, что ждать перестали мы: запрос принят и,
		// вероятно, уже оплачен. Иначе запрос не дошёл и в счёт не вошёл.
		return nil, Usage{Billed: ctx.Err() != nil}, fmt.Errorf("anthropic tool call: %w", err)
	}
	// Ответ получен — он оплачен независимо от того, вызвала модель инструмент
	// или нет, поэтому usage возвращается на всех дальнейших путях.
	usage := Usage{
		Billed: true,
		InputTokens: int(message.Usage.InputTokens +
			message.Usage.CacheCreationInputTokens + message.Usage.CacheReadInputTokens),
		OutputTokens: int(message.Usage.OutputTokens),
	}
	for _, block := range message.Content {
		if call, ok := block.AsAny().(anthropic.ToolUseBlock); ok && call.Name == tool.Name {
			return call.Input, usage, nil
		}
	}
	return nil, usage, fmt.Errorf("anthropic tool call: модель не вызвала %q", tool.Name)
}
