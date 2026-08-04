// Package llm содержит абстракцию над провайдерами языковых моделей.
package llm

import "context"

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

// Provider — минимальный контракт для общения с LLM.
// Stream отправляет запрос, вызывает onDelta для каждого фрагмента текста
// по мере генерации и возвращает полный текст ответа.
type Provider interface {
	Stream(ctx context.Context, system string, msgs []Message, onDelta func(string)) (string, error)
}
