package core

import (
	"context"
	"time"
)

// WithBackgroundTuningForTest ускоряет фоновый проход, чтобы тест наблюдал
// возобновление зависшей модерации за миллисекунды, а не за минуту.
//
// Живёт в файле _test.go и потому не попадает в собранный сервис: политика
// повторов — решение сервиса, выведенное из стоимости попытки, а не настройка
// развёртывания. Публичной ручки для неё нет намеренно; появление такой ручки
// означало бы, что оператор может отключить повторы и вернуть issue #40.
func WithBackgroundTuningForTest(tick, retry time.Duration, paidCap int) ServiceOption {
	return func(s *Service) {
		s.tick = tick
		s.moderationRetry = retry
		s.moderationPaidCap = paidCap
	}
}

// ModerateForTest прогоняет один проход модерации мимо учёта попыток — так, как
// его увидел бы опоздавший фоновый повтор. Существует ровно затем, чтобы тест
// мог проверить последнюю защиту от повторной модерации завершённых дебатов:
// снаружи пакета её не достать, а через фоновый проход не воспроизвести, потому
// что учёт попыток до неё не допускает.
func (s *Service) ModerateForTest(ctx context.Context, debateID string) {
	s.moderate(ctx, debateID)
}
