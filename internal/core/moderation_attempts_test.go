package core

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

// TestModerationAttemptBookkeepingHoldsTheSinglePass охраняет учёт, на котором
// держится весь фоновый повтор. Все его правила отказывают молча: сервис просто
// перестаёт модерировать, начинает делать это дважды или продолжает платить за
// вызовы, которые уже некому посчитать, и ни статус дебатов, ни экспорт об этом
// не говорят.
//
// Проверяется прямо по учёту, а не через сценарий, потому что часть правил из
// сценария недостижима: устаревший вызов фонового прохода требует, чтобы дебаты
// ушли вперёд между чтением списка и запуском, а опоздавшее завершение прохода —
// чтобы горутина прошлого раунда доработала после начала следующего.
func TestModerationAttemptBookkeepingHoldsTheSinglePass(t *testing.T) {
	const debateID = "dbt_bookkeeping"
	const paidCap = 3
	service := NewService(nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithBackgroundTuningForTest(time.Millisecond, 0, paidCap))

	first, ok := service.beginModerationAttempt(debateID, 1, triggerRoundClosed)
	if !ok {
		t.Fatal("первый проход раунда не начался")
	}
	// Пока проход идёт, второго не начать ни фоновым повтором, ни закрытием
	// следующего раунда: вызов модератора живёт до трёх минут, и второй проход
	// поверх него — задвоенный расход и две записи на раунд.
	if _, ok := service.beginModerationAttempt(debateID, 1, triggerResumed); ok {
		t.Fatal("повтор начался поверх живого прохода")
	}
	if _, ok := service.beginModerationAttempt(debateID, 2, triggerResumed); ok {
		t.Fatal("фоновый повтор следующего раунда начал проход поверх живого")
	}
	// Исключение — закрытие нового раунда: чтобы дебаты доиграли до него, прошлый
	// проход обязан был свою работу закончить, а признак «идёт» снимается
	// отложенным вызовом уже после неё. Без исключения проход, попавший в это
	// окно, не начался бы вовсе там, где фонового повтора нет.
	takeover, ok := service.beginModerationAttempt(debateID, 2, triggerRoundClosed)
	if !ok {
		t.Fatal("закрытие нового раунда не перебило признак «идёт»")
	}
	service.endModerationAttempt(debateID, takeover)

	service.endModerationAttempt(debateID, first)
	// Устаревший вызов не начинает ничего: фоновый проход мог прочитать список
	// дебатов до того, как они ушли вперёд.
	if _, ok := service.beginModerationAttempt(debateID, 0, triggerResumed); ok {
		t.Fatal("устаревший раунд начал проход")
	}

	second, ok := service.beginModerationAttempt(debateID, 2, triggerRoundClosed)
	if !ok {
		t.Fatal("следующий раунд не начал свой проход")
	}
	// Опоздавшая горутина прошлого раунда снимает признак «идёт» только со своего
	// прохода: иначе она открыла бы дорогу второму проходу поверх живого.
	service.endModerationAttempt(debateID, first)
	if _, ok := service.beginModerationAttempt(debateID, 2, triggerResumed); ok {
		t.Fatal("опоздавшее завершение прошлого прохода открыло дорогу второму")
	}
	service.endModerationAttempt(debateID, second)

	// Платные проходы конечны, и счёт у каждого раунда свой.
	for paid := 1; paid <= paidCap; paid++ {
		if _, ok := service.affordModelCall(debateID); !ok {
			t.Fatalf("платный проход %d отказан до потолка", paid)
		}
		service.noteModerationAdmitted(debateID)
	}
	if _, ok := service.affordModelCall(debateID); ok {
		t.Fatalf("потолок в %d платных проходов не сработал", paidCap)
	}
	// Бесплатные повторы при этом не останавливаются: ограничивать надо деньги, а
	// не живость — иначе отказ хранилища, не стоящий ни копейки, снова оставлял бы
	// дебаты ждать рестарта.
	third, ok := service.beginModerationAttempt(debateID, 2, triggerResumed)
	if !ok {
		t.Fatal("бесплатный повтор остановлен вместе с платными")
	}
	service.endModerationAttempt(debateID, third)

	fourth, ok := service.beginModerationAttempt(debateID, 3, triggerRoundClosed)
	if !ok {
		t.Fatal("новый раунд не начал проход")
	}
	service.endModerationAttempt(debateID, fourth)
	if _, ok := service.affordModelCall(debateID); !ok {
		t.Fatal("новый раунд не получил своего счёта платных проходов")
	}

	// Сломанный учёт расхода останавливает платные проходы навсегда и переживает
	// смену раунда: остаток бюджета в хранилище не сдвинулся, и каждый следующий
	// проход получал бы его заново.
	service.noteChargeLost(debateID)
	if _, ok := service.affordModelCall(debateID); ok {
		t.Fatal("после потери учёта расход продолжился")
	}
	fifth, ok := service.beginModerationAttempt(debateID, 4, triggerRoundClosed)
	if !ok {
		t.Fatal("после потери учёта остановились и бесплатные проходы")
	}
	service.endModerationAttempt(debateID, fifth)
	if _, ok := service.affordModelCall(debateID); ok {
		t.Fatal("смена раунда забыла о потерянном учёте")
	}
	if !service.chargeLost(debateID) {
		t.Fatal("потерянный учёт не виден телеметрии")
	}

	// Пока дебаты активны, их учёт остаётся; завершённые забываются, иначе карта
	// росла бы на каждые прошедшие через процесс дебаты.
	service.forgetSettledModeration(map[string]struct{}{debateID: {}})
	if _, ok := service.affordModelCall(debateID); ok {
		t.Fatal("учёт активных дебатов забыт")
	}
	service.forgetSettledModeration(nil)
	if len(service.moderations) != 0 {
		t.Fatalf("в карте осталось %d записей", len(service.moderations))
	}

	// Потеря учёта не теряется даже без заведённой записи: молча забыть её значит
	// продолжать платить вслепую.
	service.noteChargeLost("dbt_unknown")
	if _, ok := service.affordModelCall("dbt_unknown"); ok {
		t.Fatal("потеря учёта по неизвестным дебатам забыта")
	}

	// Раунд, отвергнутый как неоднозначный, повторять нечем: это состояние не
	// лечится ни повтором, ни рестартом. Повторять его значит читать протокол
	// целиком раз в минуту вечно и топить ту самую ошибку, по которой его
	// замечают (критерий отката 1 из ADR 0008).
	const hopelessID = "dbt_hopeless"
	sixth, ok := service.beginModerationAttempt(hopelessID, 1, triggerRoundClosed)
	if !ok {
		t.Fatal("первый проход неоднозначного раунда не начался")
	}
	service.endModerationAttempt(hopelessID, sixth)
	service.giveUpOnModeration(hopelessID)
	if _, ok := service.beginModerationAttempt(hopelessID, 1, triggerResumed); ok {
		t.Fatal("неоднозначный раунд повторяется")
	}
}

// TestModerationRetryBacksOff охраняет цену бесконечных повторов. Число попыток
// не ограничено намеренно, но каждая читает протокол целиком, а отказ хранилища
// задевает сразу все пишущие дебаты: без роста паузы сломанный том получал бы
// полное чтение каждых зависших дебатов каждую минуту неограниченно долго.
func TestModerationRetryBacksOff(t *testing.T) {
	const debateID = "dbt_backoff"
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	service := NewService(nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithClock(func() time.Time { return now }),
		WithBackgroundTuningForTest(time.Millisecond, time.Minute, 10))

	// Первая попытка идёт сразу — паузы ждать нечего.
	pass, ok := service.beginModerationAttempt(debateID, 1, triggerResumed)
	if !ok {
		t.Fatal("первый проход не начался")
	}
	service.endModerationAttempt(debateID, pass)

	previous := time.Duration(0)
	for try := 2; try <= 7; try++ {
		wait := time.Duration(0)
		for {
			if pass, ok := service.beginModerationAttempt(debateID, 1, triggerResumed); ok {
				service.endModerationAttempt(debateID, pass)
				break
			}
			wait += time.Second
			now = now.Add(time.Second)
			if wait > 2*moderationRetryMaxDelay {
				t.Fatalf("попытка %d не возобновилась даже спустя %s", try, wait)
			}
		}
		// Пауза растёт, пока не упрётся в потолок, и дальше держится на нём.
		switch {
		case wait > moderationRetryMaxDelay:
			t.Fatalf("пауза %s превысила потолок %s", wait, moderationRetryMaxDelay)
		case try > 2 && wait <= previous && previous < moderationRetryMaxDelay:
			t.Fatalf("пауза не выросла: попытка %d ждала %s, предыдущая %s", try, wait, previous)
		}
		previous = wait
	}
	if previous != moderationRetryMaxDelay {
		t.Fatalf("пауза остановилась на %s, ожидался потолок %s", previous, moderationRetryMaxDelay)
	}
}
