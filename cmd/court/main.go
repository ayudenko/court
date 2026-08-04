// court — CLI для дебатов AI-агентов: несколько моделей обсуждают один вопрос
// раундами, модератор фиксирует консенсус и выносит итоговое решение.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"court/internal/config"
	"court/internal/debate"
	"court/internal/llm"
)

const defaultMaxTokens = 4096

var agentColors = []string{
	"\033[36m", // cyan
	"\033[33m", // yellow
	"\033[35m", // magenta
	"\033[32m", // green
	"\033[34m", // blue
	"\033[31m", // red
}

const (
	colorReset = "\033[0m"
	colorBold  = "\033[1m"
	colorDim   = "\033[2m"
)

func main() {
	configPath := flag.String("config", "config.yaml", "путь к конфигурации дебатов")
	transcriptPath := flag.String("out", "", "сохранить протокол дискуссии в Markdown-файл")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Использование: court [флаги] \"вопрос для обсуждения\"\n\nФлаги:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	question := strings.TrimSpace(strings.Join(flag.Args(), " "))
	if question == "" {
		flag.Usage()
		os.Exit(2)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal(err)
	}

	moderator, err := buildAgent(cfg.Moderator)
	if err != nil {
		fatal(err)
	}
	agents := make([]debate.Agent, 0, len(cfg.Agents))
	for _, ac := range cfg.Agents {
		a, err := buildAgent(ac)
		if err != nil {
			fatal(err)
		}
		agents = append(agents, a)
	}

	colors := map[string]string{moderator.Name: colorBold}
	for i, a := range agents {
		colors[a.Name] = agentColors[i%len(agentColors)]
	}

	d := &debate.Debate{
		Question:  question,
		Rounds:    cfg.Rounds,
		Agents:    agents,
		Moderator: moderator,
		Events: debate.Events{
			OnRoundStart: func(round, total int) {
				fmt.Printf("\n%s========== Раунд %d из %d ==========%s\n", colorBold, round, total, colorReset)
			},
			OnTurnStart: func(speaker string) {
				fmt.Printf("\n%s%s[%s]%s\n", colorBold, colors[speaker], speaker, colorReset)
			},
			OnDelta: func(speaker, delta string) {
				fmt.Print(colors[speaker] + delta + colorReset)
			},
			OnTurnEnd: func(string) { fmt.Println() },
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	fmt.Printf("%sВопрос:%s %s\n", colorBold, colorReset, question)
	fmt.Printf("%sУчастники: %s. Модератор: %s.%s\n",
		colorDim, joinNames(agents), moderator.Name, colorReset)

	result, err := d.Run(ctx)
	if err != nil {
		fatal(err)
	}

	fmt.Printf("\n%s========== Итог ==========%s\n", colorBold, colorReset)
	if result.Consensus {
		fmt.Printf("Консенсус достигнут в раунде %d.\n", result.FinalRound)
	} else {
		fmt.Printf("Консенсус не зафиксирован, проведено раундов: %d.\n", result.FinalRound)
	}

	if *transcriptPath != "" {
		if err := saveTranscript(*transcriptPath, question, result); err != nil {
			fatal(err)
		}
		fmt.Printf("%sПротокол сохранён: %s%s\n", colorDim, *transcriptPath, colorReset)
	}
}

func buildAgent(ac config.AgentConfig) (debate.Agent, error) {
	apiKey := ""
	if ac.APIKeyEnv != "" {
		apiKey = os.Getenv(ac.APIKeyEnv)
		if apiKey == "" {
			return debate.Agent{}, fmt.Errorf("агент %q: переменная окружения %s пуста", ac.Name, ac.APIKeyEnv)
		}
	}
	maxTokens := ac.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}

	var provider llm.Provider
	switch ac.Provider {
	case "anthropic":
		provider = llm.NewAnthropicProvider(apiKey, ac.Model, maxTokens)
	case "openai":
		provider = llm.NewOpenAICompatProvider(apiKey, ac.BaseURL, ac.Model, maxTokens)
	default:
		return debate.Agent{}, fmt.Errorf("агент %q: неизвестный провайдер %q", ac.Name, ac.Provider)
	}
	return debate.Agent{Name: ac.Name, Persona: ac.Persona, Provider: provider}, nil
}

func saveTranscript(path, question string, result *debate.Result) error {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Дебаты: %s\n\n", question)
	fmt.Fprintf(&sb, "_%s_\n\n", time.Now().Format("2006-01-02 15:04"))
	currentRound := 0
	for _, e := range result.Transcript {
		if e.Round != currentRound {
			currentRound = e.Round
			fmt.Fprintf(&sb, "## Раунд %d\n\n", currentRound)
		}
		fmt.Fprintf(&sb, "### %s\n\n%s\n\n", e.Speaker, strings.TrimSpace(e.Text))
	}
	return os.WriteFile(path, []byte(sb.String()), 0o644)
}

func joinNames(agents []debate.Agent) string {
	names := make([]string, len(agents))
	for i, a := range agents {
		names[i] = a.Name
	}
	return strings.Join(names, ", ")
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "Ошибка: %v\n", err)
	os.Exit(1)
}
