package moderator

import (
	"context"
	"testing"

	"court/internal/llm"
)

// fakeProvider возвращает заранее заданный текст.
type fakeProvider struct{ text string }

func (f fakeProvider) Stream(_ context.Context, _ string, _ []llm.Message, _ func(string)) (string, error) {
	return f.text, nil
}

func TestCheckRoundConsensus(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{
			name: "консенсус при пустом списке",
			text: "Итог раунда: участники сошлись во всём.\n\nОТКРЫТЫЕ ВОПРОСЫ: НЕТ\n\nКОНСЕНСУС: ДА",
			want: true,
		},
		{
			name: "маркер ДА, но список не пуст — не консенсус",
			text: "Итог раунда: согласие по архитектуре.\n\nОТКРЫТЫЕ ВОПРОСЫ:\n1. Механизм пополнения vault.\n\nКОНСЕНСУС: ДА",
			want: false,
		},
		{
			name: "маркер ДА без раздела открытых вопросов — не консенсус",
			text: "Итог раунда: всё хорошо.\n\nКОНСЕНСУС: ДА",
			want: false,
		},
		{
			name: "явный НЕТ",
			text: "Итог раунда: спорят.\n\nОТКРЫТЫЕ ВОПРОСЫ:\n1. Всё.\n\nКОНСЕНСУС: НЕТ",
			want: false,
		},
		{
			name: "markdown-обёртка вокруг НЕТ",
			text: "## Итог\n\n**Открытые вопросы:** нет.\n\nКОНСЕНСУС: ДА",
			want: true,
		},
		{
			name: "заголовок отдельной строкой, ниже пункты",
			text: "Итог.\n\n**ОТКРЫТЫЕ ВОПРОСЫ:**\n\n1. Пороговые значения SLO.\n2. Эвристики роутинга.\n\nКОНСЕНСУС: ДА",
			want: false,
		},
		{
			name: "заголовок отдельной строкой, ниже НЕТ",
			text: "Итог.\n\nОТКРЫТЫЕ ВОПРОСЫ:\n\nНет.\n\nКОНСЕНСУС: ДА",
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := New("Модератор", fakeProvider{text: tc.text})
			got, summary, err := m.CheckRound(context.Background(), "вопрос", "протокол", 1)
			if err != nil {
				t.Fatalf("CheckRound: %v", err)
			}
			if summary != tc.text {
				t.Errorf("summary должен возвращаться как есть")
			}
			if got != tc.want {
				t.Errorf("consensus = %v, ожидалось %v\nтекст:\n%s", got, tc.want, tc.text)
			}
		})
	}
}
