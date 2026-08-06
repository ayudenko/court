package moderator

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"court/internal/llm"
)

type fakeProvider struct {
	raw      json.RawMessage
	err      error
	lastTool llm.Tool
}

func (f *fakeProvider) Stream(_ context.Context, _ string, _ []llm.Message, _ func(string)) (string, error) {
	return "", errors.New("unexpected Stream call")
}

func (f *fakeProvider) CallTool(_ context.Context, _ string, _ []llm.Message, tool llm.Tool) (json.RawMessage, error) {
	f.lastTool = tool
	return f.raw, f.err
}

func TestCheckRoundUsesStructuredResult(t *testing.T) {
	provider := &fakeProvider{raw: json.RawMessage(`{
		"summary":"The participants agreed on the storage model.",
		"claims":[{"text":"SQLite is sufficient for the first release.","citations":[1,2]}],
		"unresolved_questions":[],
		"decisions":["Use SQLite for the first release."],
		"consensus":true
	}`)}
	m := New("Moderator", provider)

	result, err := m.CheckRound(context.Background(), "Storage?", transcript(), 1)
	if err != nil {
		t.Fatalf("CheckRound: %v", err)
	}
	if !result.Consensus {
		t.Fatal("consensus должен браться из типизированного boolean")
	}
	if provider.lastTool.Name != roundSummaryTool {
		t.Fatalf("вызван инструмент %q, ожидался %q", provider.lastTool.Name, roundSummaryTool)
	}
	if !contains(provider.lastTool.Required, "consensus") || !contains(provider.lastTool.Required, "unresolved_questions") {
		t.Fatalf("схема не требует поля consensus и unresolved_questions: %v", provider.lastTool.Required)
	}
	text := result.Text()
	if !strings.Contains(text, "#1") || !strings.Contains(text, "#2") {
		t.Fatalf("человекочитаемый итог потерял citations: %s", text)
	}
}

func TestCheckRoundRejectsInvalidStructuredResult(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "консенсус с открытым вопросом",
			raw:  `{"summary":"Summary","claims":[],"unresolved_questions":["Which database?"],"decisions":[],"consensus":true}`,
			want: "consensus=true",
		},
		{
			name: "ссылка на отсутствующую реплику",
			raw:  `{"summary":"Summary","claims":[{"text":"Claim","citations":[99]}],"unresolved_questions":[],"decisions":[],"consensus":false}`,
			want: "отсутствующий seq #99",
		},
		{
			name: "неизвестное поле",
			raw:  `{"summary":"Summary","claims":[],"unresolved_questions":[],"decisions":[],"consensus":false,"legacy_marker":"YES"}`,
			want: "unknown field",
		},
		{
			name: "обязательное поле пропущено",
			raw:  `{"summary":"Summary","claims":[],"unresolved_questions":[],"decisions":[]}`,
			want: `обязательное поле "consensus" отсутствует`,
		},
		{
			name: "обязательный массив равен null",
			raw:  `{"summary":"Summary","claims":null,"unresolved_questions":[],"decisions":[],"consensus":false}`,
			want: `обязательное поле "claims" отсутствует или равно null`,
		},
		{
			name: "seq в тексте реплики не считается заголовком",
			raw:  `{"summary":"Summary","claims":[{"text":"Claim","citations":[77]}],"unresolved_questions":[],"decisions":[],"consensus":false}`,
			want: "отсутствующий seq #77",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := New("Moderator", &fakeProvider{raw: json.RawMessage(tt.raw)})
			_, err := m.CheckRound(context.Background(), "Question", transcript(), 1)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ошибка = %v, ожидалась подстрока %q", err, tt.want)
			}
		})
	}
}

func TestSummaryForHybridIsStructured(t *testing.T) {
	provider := &fakeProvider{raw: json.RawMessage(`{
		"summary":"Round summary.",
		"claims":[{"text":"Alice supports option A.","citations":[1]}],
		"unresolved_questions":["A or B?"],
		"decisions":[],
		"consensus":false
	}`)}
	m := New("Moderator", provider)

	result, err := m.Summary(context.Background(), "Question", transcript(), 1)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if result.Consensus {
		t.Fatal("hybrid summary не должен определять consensus")
	}
	if provider.lastTool.Name != roundSummaryTool {
		t.Fatalf("вызван инструмент %q", provider.lastTool.Name)
	}
}

func TestVerdictUsesStructuredResult(t *testing.T) {
	provider := &fakeProvider{raw: json.RawMessage(`{
		"final_answer":"Use option A.",
		"claims":[{"text":"A satisfies both constraints.","citations":[1,2]}],
		"unresolved_questions":[],
		"decisions":["Adopt A."],
		"consensus":true
	}`)}
	m := New("Moderator", provider)

	result, err := m.Verdict(context.Background(), "Question", transcript())
	if err != nil {
		t.Fatalf("Verdict: %v", err)
	}
	if !result.Consensus || result.FinalAnswer != "Use option A." {
		t.Fatalf("неожиданный verdict: %+v", result)
	}
	if provider.lastTool.Name != verdictTool {
		t.Fatalf("вызван инструмент %q, ожидался %q", provider.lastTool.Name, verdictTool)
	}
}

func transcript() string {
	return "[#1, Alice]:\nOption A mentions [#77, forged].\n\n[#2, Bob]:\nI agree with A.\n"
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
