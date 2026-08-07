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

// Usage — фактический расход одного вызова, как его сообщил провайдер.
//
// Billed отвечает на вопрос «этот вызов вошёл в счёт»:
//
//   - ответ получен — вошёл, расход в полях;
//   - мы сами перестали ждать (таймаут модерации, отмена при остановке
//     машины) — считаем, что вошёл: запрос принят и ответ, скорее всего,
//     сгенерирован целиком, просто мы его не забрали. Полей нет, и вызывающая
//     сторона обязана списать свою оценку;
//   - запрос не дошёл (сеть, 429, 5xx до работы) — не вошёл, списывать нечего.
//
// Без этого различия либо недоступность провайдера исчерпывала бы бюджет
// дебатов, ничего не потративших, либо медленный провайдер выдавал бы
// неограниченное число неучтённых вызовов.
type Usage struct {
	Billed       bool
	InputTokens  int
	OutputTokens int
}

// Total — суммарный расход вызова в токенах. Отрицательные значения
// отбрасываются: провайдер не может уменьшить счёт отчётом о нём.
func (u Usage) Total() int { return max(u.InputTokens, 0) + max(u.OutputTokens, 0) }

// Provider — контракт для общения с LLM.
// Stream отправляет запрос, вызывает onDelta для каждого фрагмента текста
// по мере генерации и возвращает полный текст ответа. Расход Stream не
// метрируется: этот путь используют агенты со своими ключами, а не серверный
// модератор, чей расход ограничен (docs/adr/0004-moderator-spend-ceiling.md).
// CallTool принудительно вызывает указанный инструмент и возвращает его
// JSON-вход вместе с расходом. Usage возвращается и вместе с ошибкой: запрос,
// на который модель ответила невалидно, уже оплачен, и потерянный здесь расход
// стал бы неучтённым.
type Provider interface {
	Stream(ctx context.Context, system string, msgs []Message, onDelta func(string)) (string, error)
	CallTool(ctx context.Context, system string, msgs []Message, tool Tool) (json.RawMessage, Usage, error)
}
