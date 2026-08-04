// Package config загружает описание дебатов из YAML-файла.
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// AgentConfig — описание одного участника (или модератора).
type AgentConfig struct {
	Name      string `yaml:"name"`
	Provider  string `yaml:"provider"`    // "anthropic" | "openai"
	Model     string `yaml:"model"`       // id модели у провайдера
	Persona   string `yaml:"persona"`     // системная инструкция: позиция/характер
	BaseURL   string `yaml:"base_url"`    // для openai-совместимых API
	APIKeyEnv string `yaml:"api_key_env"` // имя env-переменной с ключом
	MaxTokens int64  `yaml:"max_tokens"`
}

// Config — вся конфигурация дебатов.
type Config struct {
	Rounds    int           `yaml:"rounds"` // максимум раундов дискуссии
	Moderator AgentConfig   `yaml:"moderator"`
	Agents    []AgentConfig `yaml:"agents"`
}

// Load читает и валидирует конфигурацию.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("чтение конфига: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("разбор конфига: %w", err)
	}
	if cfg.Rounds <= 0 {
		cfg.Rounds = 3
	}
	if len(cfg.Agents) < 2 {
		return nil, fmt.Errorf("нужно минимум два агента, в конфиге %d", len(cfg.Agents))
	}
	all := append([]AgentConfig{cfg.Moderator}, cfg.Agents...)
	for i := range all {
		a := all[i]
		if a.Name == "" {
			return nil, fmt.Errorf("у агента №%d не задано имя", i)
		}
		if a.Provider != "anthropic" && a.Provider != "openai" {
			return nil, fmt.Errorf("агент %q: неизвестный провайдер %q (ожидается anthropic или openai)", a.Name, a.Provider)
		}
		if a.Model == "" {
			return nil, fmt.Errorf("агент %q: не задана модель", a.Name)
		}
	}
	return &cfg, nil
}
