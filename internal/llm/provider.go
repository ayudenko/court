// Package llm содержит абстракцию над провайдерами языковых моделей.
package llm

import (
	"context"
	"encoding/json"
)

// Role — роль сообщения в диалоге.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message — одно сообщение диалога, независимое от провайдера.
type Message struct {
	Role    Role
	Content string
}

// Tool описывает единственный структурированный результат, который модель
// обязана вернуть вызовом инструмента. Properties и Required задают корневой
// JSON-object; провайдер запрещает дополнительные поля на верхнем уровне.
type Tool struct {
	Name        string
	Description string
	Properties  map[string]any
	Required    []string
}

// Provider — контракт для общения с LLM.
// Stream отправляет запрос, вызывает onDelta для каждого фрагмента текста
// по мере генерации и возвращает полный текст ответа.
// CallTool принудительно вызывает указанный инструмент и возвращает его JSON-вход.
type Provider interface {
	Stream(ctx context.Context, system string, msgs []Message, onDelta func(string)) (string, error)
	CallTool(ctx context.Context, system string, msgs []Message, tool Tool) (json.RawMessage, error)
}
