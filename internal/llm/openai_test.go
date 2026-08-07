package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCallToolReportsUsageIncludingWhenTheModelIgnoresTheTool проверяет слой, в
// котором расход рождается. Потолок расхода модератора считает то, что вернул
// провайдер, поэтому ответ без вызова инструмента обязан приносить usage: он
// оплачен так же, как удачный (docs/adr/0004-moderator-spend-ceiling.md).
func TestCallToolReportsUsageIncludingWhenTheModelIgnoresTheTool(t *testing.T) {
	tool := Tool{
		Name:        "submit_round_summary",
		Description: "test tool",
		Properties:  map[string]any{"summary": map[string]any{"type": "string"}},
		Required:    []string{"summary"},
	}

	tests := []struct {
		name       string
		body       string
		wantErr    bool
		wantResult string
	}{
		{
			name: "инструмент вызван",
			body: `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant",
			        "tool_calls":[{"id":"t1","type":"function","function":{"name":"submit_round_summary",
			        "arguments":"{\"summary\":\"ok\"}"}}]},"finish_reason":"tool_calls"}],
			        "usage":{"prompt_tokens":1234,"completion_tokens":56}}`,
			wantResult: `{"summary":"ok"}`,
		},
		{
			name: "модель ответила текстом вместо инструмента",
			body: `{"id":"c2","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant",
			        "content":"я не буду вызывать инструмент"},"finish_reason":"stop"}],
			        "usage":{"prompt_tokens":1234,"completion_tokens":56}}`,
			wantErr: true,
		},
		{
			name: "ответ без вариантов",
			body: `{"id":"c3","object":"chat.completion","choices":[],
			        "usage":{"prompt_tokens":1234,"completion_tokens":56}}`,
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if _, err := w.Write([]byte(test.body)); err != nil {
					t.Errorf("write response: %v", err)
				}
			}))
			defer server.Close()

			provider := NewOpenAICompatProvider("test-key", server.URL, "test-model", 4096)
			raw, usage, err := provider.CallTool(context.Background(), "system", []Message{
				{Role: RoleUser, Content: "prompt"},
			}, tool)
			if test.wantErr != (err != nil) {
				t.Fatalf("err = %v, ожидалась ошибка: %t", err, test.wantErr)
			}
			if !test.wantErr && string(raw) != test.wantResult {
				t.Fatalf("результат = %s, ожидался %s", raw, test.wantResult)
			}
			if !usage.Billed {
				t.Fatal("полученный ответ обязан быть помечен как оплаченный")
			}
			if usage.InputTokens != 1234 || usage.OutputTokens != 56 {
				t.Fatalf("usage = %+v, ожидался {1234 56}", usage)
			}
		})
	}
}

// TestCallToolReportsNoUsageWhenTheRequestFails: запрос, не доехавший до ответа,
// не оплачен, и пометка Responded обязана это отражать — иначе сервис списал бы
// за него оценку.
func TestCallToolReportsNoUsageWhenTheRequestFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	provider := NewOpenAICompatProvider("test-key", server.URL, "test-model", 4096)
	_, usage, err := provider.CallTool(context.Background(), "system",
		[]Message{{Role: RoleUser, Content: "prompt"}},
		Tool{Name: "t", Properties: map[string]any{}, Required: []string{}})
	if err == nil {
		t.Fatal("ответ 500 должен быть ошибкой")
	}
	if usage.Billed || usage.Total() != 0 {
		t.Fatalf("неудачный запрос принёс расход: %+v", usage)
	}
}
