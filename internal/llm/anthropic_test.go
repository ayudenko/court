package llm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAnthropicCallToolReportsUsage покрывает провайдера по умолчанию
// (COURT_MODERATOR_PROVIDER=anthropic). Потолок расхода модератора считает
// именно эти числа, и если маппинг usage тихо сломается на обновлении SDK, все
// вызовы станут бесплатными для счётчика, а потолок перестанет существовать —
// при полностью зелёном `make check`, потому что остальные тесты используют
// фейковый модератор (docs/adr/0004-moderator-spend-ceiling.md).
func TestAnthropicCallToolReportsUsage(t *testing.T) {
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
		wantInput  int
	}{
		{
			name: "инструмент вызван",
			body: `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5",
			        "content":[{"type":"tool_use","id":"tu_1","name":"submit_round_summary",
			        "input":{"summary":"ok"}}],"stop_reason":"tool_use",
			        "usage":{"input_tokens":1000,"output_tokens":40}}`,
			wantResult: `{"summary":"ok"}`,
			wantInput:  1000,
		},
		{
			name: "модель ответила текстом вместо инструмента",
			body: `{"id":"msg_2","type":"message","role":"assistant","model":"claude-opus-5",
			        "content":[{"type":"text","text":"не буду"}],"stop_reason":"end_turn",
			        "usage":{"input_tokens":1000,"output_tokens":40}}`,
			wantErr:   true,
			wantInput: 1000,
		},
		{
			// Кэш-токены тоже в счёте, поэтому обязаны попадать во входные:
			// потерянные, они занизили бы расход втрое на длинном протоколе.
			name: "кэш-токены учтены во входных",
			body: `{"id":"msg_3","type":"message","role":"assistant","model":"claude-opus-5",
			        "content":[{"type":"tool_use","id":"tu_3","name":"submit_round_summary",
			        "input":{"summary":"ok"}}],"stop_reason":"tool_use",
			        "usage":{"input_tokens":1000,"output_tokens":40,
			        "cache_creation_input_tokens":300,"cache_read_input_tokens":700}}`,
			wantResult: `{"summary":"ok"}`,
			wantInput:  2000,
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
			t.Setenv("ANTHROPIC_BASE_URL", server.URL)

			provider := NewAnthropicProvider("test-key", "claude-opus-5", 4096)
			raw, usage, err := provider.CallTool(context.Background(), "system",
				[]Message{{Role: RoleUser, Content: "prompt"}}, tool)
			if test.wantErr != (err != nil) {
				t.Fatalf("err = %v, ожидалась ошибка: %t", err, test.wantErr)
			}
			if !test.wantErr && string(raw) != test.wantResult {
				t.Fatalf("результат = %s, ожидался %s", raw, test.wantResult)
			}
			if !usage.Billed {
				t.Fatal("полученный ответ обязан быть помечен как оплаченный")
			}
			if usage.InputTokens != test.wantInput || usage.OutputTokens != 40 {
				t.Fatalf("usage = %+v, ожидались входные %d и выходные 40", usage, test.wantInput)
			}
		})
	}
}

// TestAnthropicCallToolChargesOurOwnCancellation: запрос, который мы перестали
// ждать, провайдер уже принял и, вероятно, посчитал — такой вызов помечается
// оплаченным без чисел, чтобы сервис списал за него оценку. Запрос, не дошедший
// до провайдера, наоборот, в счёт не входит.
func TestAnthropicCallToolChargesOurOwnCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	t.Setenv("ANTHROPIC_BASE_URL", server.URL)
	provider := NewAnthropicProvider("test-key", "claude-opus-5", 4096)
	tool := Tool{Name: "t", Properties: map[string]any{}, Required: []string{}}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, usage, err := provider.CallTool(cancelled, "system",
		[]Message{{Role: RoleUser, Content: "prompt"}}, tool); err == nil {
		t.Fatal("отменённый запрос должен быть ошибкой")
	} else if !usage.Billed {
		t.Fatalf("отменённый нами вызов не помечен оплаченным: %+v", usage)
	}

	if _, usage, err := provider.CallTool(context.Background(), "system",
		[]Message{{Role: RoleUser, Content: "prompt"}}, tool); err == nil {
		t.Fatal("ответ 500 должен быть ошибкой")
	} else if usage.Billed || usage.Total() != 0 {
		t.Fatalf("не дошедший запрос принёс расход: %+v", usage)
	}
}
