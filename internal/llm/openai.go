package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v2"
	"github.com/openai/openai-go/v2/option"
	"github.com/openai/openai-go/v2/shared"
)

// OpenAICompatProvider — провайдер для OpenAI и любых OpenAI-совместимых API
// (Gemini через compat-endpoint, Ollama, OpenRouter и т.д.) — задаётся baseURL.
type OpenAICompatProvider struct {
	client    openai.Client
	model     string
	maxTokens int64
}

// NewOpenAICompatProvider создаёт провайдера. Если apiKey пуст,
// SDK возьмёт ключ из OPENAI_API_KEY. Пустой baseURL — api.openai.com.
func NewOpenAICompatProvider(apiKey, baseURL, model string, maxTokens int64) *OpenAICompatProvider {
	var opts []option.RequestOption
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return &OpenAICompatProvider{
		client:    openai.NewClient(opts...),
		model:     model,
		maxTokens: maxTokens,
	}
}

func (p *OpenAICompatProvider) Stream(ctx context.Context, system string, msgs []Message, onDelta func(string)) (string, error) {
	var messages []openai.ChatCompletionMessageParamUnion
	if system != "" {
		messages = append(messages, openai.SystemMessage(system))
	}
	for _, m := range msgs {
		switch m.Role {
		case RoleAssistant:
			messages = append(messages, openai.AssistantMessage(m.Content))
		default:
			messages = append(messages, openai.UserMessage(m.Content))
		}
	}

	stream := p.client.Chat.Completions.NewStreaming(ctx, openai.ChatCompletionNewParams{
		Model:               p.model,
		Messages:            messages,
		MaxCompletionTokens: openai.Int(p.maxTokens),
	})
	var sb strings.Builder
	for stream.Next() {
		chunk := stream.Current()
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta.Content
		if delta == "" {
			continue
		}
		sb.WriteString(delta)
		if onDelta != nil {
			onDelta(delta)
		}
	}
	if err := stream.Err(); err != nil {
		return sb.String(), fmt.Errorf("openai stream: %w", err)
	}
	return sb.String(), nil
}

// CallTool заставляет OpenAI-совместимую модель вернуть аргументы function tool.
func (p *OpenAICompatProvider) CallTool(ctx context.Context, system string, msgs []Message, tool Tool) (json.RawMessage, Usage, error) {
	var messages []openai.ChatCompletionMessageParamUnion
	if system != "" {
		messages = append(messages, openai.SystemMessage(system))
	}
	for _, m := range msgs {
		switch m.Role {
		case RoleAssistant:
			messages = append(messages, openai.AssistantMessage(m.Content))
		default:
			messages = append(messages, openai.UserMessage(m.Content))
		}
	}

	parameters := shared.FunctionParameters{
		"type":                 "object",
		"properties":           tool.Properties,
		"required":             tool.Required,
		"additionalProperties": false,
	}
	completion, err := p.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:               p.model,
		Messages:            messages,
		MaxCompletionTokens: openai.Int(p.maxTokens),
		Tools: []openai.ChatCompletionToolUnionParam{
			openai.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
				Name:        tool.Name,
				Description: openai.String(tool.Description),
				Parameters:  parameters,
				Strict:      openai.Bool(true),
			}),
		},
		ToolChoice: openai.ToolChoiceOptionFunctionToolChoice(
			openai.ChatCompletionNamedToolChoiceFunctionParam{Name: tool.Name},
		),
	})
	if err != nil {
		// ctx.Err() != nil означает, что ждать перестали мы: запрос принят и,
		// вероятно, уже оплачен. Иначе запрос не дошёл и в счёт не вошёл.
		return nil, Usage{Billed: ctx.Err() != nil}, fmt.Errorf("openai tool call: %w", err)
	}
	// Ответ получен и оплачен — usage возвращается и на путях с ошибкой.
	usage := Usage{
		Billed:       true,
		InputTokens:  int(completion.Usage.PromptTokens),
		OutputTokens: int(completion.Usage.CompletionTokens),
	}
	if len(completion.Choices) == 0 {
		return nil, usage, fmt.Errorf("openai tool call: пустой ответ")
	}
	for _, call := range completion.Choices[0].Message.ToolCalls {
		if fn, ok := call.AsAny().(openai.ChatCompletionMessageFunctionToolCall); ok && fn.Function.Name == tool.Name {
			return json.RawMessage(fn.Function.Arguments), usage, nil
		}
	}
	return nil, usage, fmt.Errorf("openai tool call: модель не вызвала %q", tool.Name)
}
