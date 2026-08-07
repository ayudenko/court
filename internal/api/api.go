// Package api — REST API и SSE-поток событий дебатов.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"court/internal/core"
	"court/internal/protocol"
	"court/internal/ratelimit"
	"court/internal/store"
)

// MaxWaitSec — потолок long-poll ожидания очереди.
const MaxWaitSec = 120

const (
	// MaxConcurrentExports — потолок одновременных экспортов на процесс.
	//
	// Это ограничение памяти, а не политика клиента: один экспорт держит
	// протокол дебатов и его JSONL-представление одновременно, а слот на адрес
	// такую сумму не ограничивает — адресов у нападающего столько, сколько
	// разрешит фронт. Выведен из MaxExportBytes: столько артефактов предельного
	// размера процесс переживает одновременно.
	MaxConcurrentExports = 4
	// MaxExportBytes — верхняя оценка одного артефакта, из которой выведен
	// потолок. Считается по собственным лимитам сервиса: MaxParticipants раз
	// MaxRounds реплик по MaxArgumentLen байт, каждый из которых в худшем случае
	// управляющий и уезжает в JSON шестью символами, плюс модерация и обвязка
	// записей. Добавление полей в записи требует пересчитать обе константы, а не
	// унаследовать их.
	MaxExportBytes = 16 << 20
	// exportsPerAddress — доля потолка, доступная одному адресу.
	//
	// Потолок общий, поэтому без доли одного адреса с MaxConcurrentExports
	// переставшими читать соединениями хватает, чтобы маршрут отвечал 503 всем
	// остальным, — и отказ ничего не стоит, поэтому освободившийся слот снова
	// достаётся тому, кто чаще спрашивает. Доля не делает атаку невозможной, но
	// перестаёт отдавать весь маршрут одному соединению.
	exportsPerAddress = MaxConcurrentExports / 2
	// defaultSlowExportThreshold — порог, выше которого сборка попадает в лог
	// как наблюдаемый признак того, что артефакт перерос маршрут.
	defaultSlowExportThreshold = 2 * time.Second
	// exportWriteTimeout — предел на запись готового артефакта.
	//
	// Слот потолка держится до конца записи, иначе он не ограничивает память:
	// собранные байты живут, пока их не отдали. Значит, у записи обязан быть
	// предел, иначе соединение, переставшее читать, занимает слот навсегда.
	// Обрыв по этому пределу не может притвориться короткими дебатами: ответ
	// объявляет Content-Length, и клиент увидит недочитанное тело.
	//
	// Значение — компромисс: предел ограничивает и то, как долго переставшие
	// читать соединения держат маршрут занятым, и то, насколько медленному
	// клиенту хватит времени забрать предельный по размеру артефакт.
	exportWriteTimeout = 30 * time.Second
	// exportRetryAfterSec — подсказка клиенту, отвергнутому потолком. Названо
	// худшее время удержания слота, а не типичное: сборка занимает миллисекунды,
	// но слот освобождается только по завершении записи, и подсказка «через 5 с»
	// была бы ложью ровно в том случае, ради которого она нужна.
	exportRetryAfterSec = int(exportWriteTimeout / time.Second)
)

// Server — REST-обвязка над ядром.
type Server struct {
	svc               *core.Service
	log               *slog.Logger
	limiter           *ratelimit.Limiter
	heartbeatInterval time.Duration
	exports           chan struct{}
	exportsByAddress  addressShare
	slowExport        time.Duration
	deadlineWarning   sync.Once
}

// addressShare считает занятые слоты по адресам. Живёт здесь, а не в лимитере:
// делится ресурс процесса, а не бюджет клиента, и общий потолок остаётся
// авторитетом. Записей не больше числа одновременных экспортов, поэтому таблица
// не растёт и чистить её нечем — слот удаляется вместе с последним держателем.
type addressShare struct {
	mu    sync.Mutex
	held  map[string]int
	limit int
}

func (s *addressShare) acquire(address string) (release func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held[address] >= s.limit {
		return func() {}, false
	}
	if s.held == nil {
		s.held = make(map[string]int, MaxConcurrentExports)
	}
	s.held[address]++
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.held[address] <= 1 {
				delete(s.held, address)
				return
			}
			s.held[address]--
		})
	}, true
}

// New создаёт сервер API. Нулевой limiter означает «без лимитов».
func New(svc *core.Service, log *slog.Logger, limiter *ratelimit.Limiter) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		svc: svc, log: log, limiter: limiter, heartbeatInterval: 25 * time.Second,
		exports:          make(chan struct{}, MaxConcurrentExports),
		exportsByAddress: addressShare{limit: exportsPerAddress},
		slowExport:       defaultSlowExportThreshold,
	}
}

// Routes регистрирует маршруты API на mux.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("POST /api/agents", s.handleRegister)
	mux.HandleFunc("GET /api/agents/me", s.auth(s.handleMe))
	mux.HandleFunc("POST /api/agents/me/credentials", s.auth(s.handleIssueCredential))
	mux.HandleFunc("GET /api/agents/me/credentials", s.auth(s.handleListCredentials))
	mux.HandleFunc("DELETE /api/agents/me/credentials/{id}", s.auth(s.handleRevokeCredential))

	mux.HandleFunc("POST /api/debates", s.auth(s.handleCreateDebate))
	mux.HandleFunc("GET /api/debates", s.handleListDebates)
	mux.HandleFunc("GET /api/debates/{id}", s.handleGetDebate)
	mux.HandleFunc("DELETE /api/debates/{id}", s.auth(s.handleDeleteDebate))
	mux.HandleFunc("GET /api/debates/{id}/messages", s.handleMessages)
	mux.HandleFunc("GET /api/debates/{id}/export", s.limitIPStream(s.handleExport))
	mux.HandleFunc("POST /api/debates/{id}/join", s.auth(s.handleJoin))
	mux.HandleFunc("POST /api/debates/{id}/start", s.auth(s.handleStart))
	mux.HandleFunc("GET /api/debates/{id}/turn", s.auth(s.limitAgentStream(s.handleTurn)))
	mux.HandleFunc("POST /api/debates/{id}/messages", s.auth(s.handlePost))
	mux.HandleFunc("GET /api/debates/{id}/events", s.limitIPStream(s.handleEvents))
}

// --- Лимиты ---
//
// Лимиты живут на границе HTTP, а не в ядре: ключ лимита — адрес клиента или
// стабильный agent_id, и ни то ни другое ядру не известно.
// См. docs/adr/0003-http-rate-limiting.md.

// refundInvalid возвращает потраченный лимит, если ядро отклонило запрос по
// валидации: ничего не создано, модератор не тронут, а агент, который шлёт
// кривые аргументы, иначе запирает сам себя на час. Нераспознанное тело
// (`decode`) остаётся оплаченным: иначе поток мусора на неаутентифицированную
// регистрацию бесплатен, а лимит на ней — единственная защита.
// Потолок действующих ключей возвращается тоже: строка не создана, работы не
// сделано, а состояние потолка и так читается через список ключей — проверять
// его вслепую незачем. Без возврата агент, упёршийся в потолок, обменивал бы
// свой часовой бюджет на 429 и не мог выпустить замену сразу после того, как
// освободил слот отзывом утёкшего ключа — то есть ровно в тот момент, ради
// которого ротация и существует.
func refundInvalid(grant *ratelimit.Grant, err error) {
	if errors.Is(err, core.ErrValidation) || errors.Is(err, store.ErrTooManyCredentials) {
		grant.Refund()
	}
}

// limitAgentStream ограничивает одновременные long-poll агента.
func (s *Server) limitAgentStream(next authedHandler) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, agent core.Agent) {
		release, err := s.limiter.AcquireStream(agent.ID, s.limiter.ClientIP(r))
		defer release()
		if err != nil {
			writeError(w, err)
			return
		}
		next(w, r, agent)
	}
}

// limitIPStream ограничивает одновременные SSE-подписки: поток событий открыт
// всем, поэтому ключом может быть только адрес.
func (s *Server) limitIPStream(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		release, err := s.limiter.AcquireStream("", s.limiter.ClientIP(r))
		defer release()
		if err != nil {
			writeError(w, err)
			return
		}
		next(w, r)
	}
}

// --- Аутентификация ---

type authedHandler func(w http.ResponseWriter, r *http.Request, agent core.Agent)

// AgentFromRequest аутентифицирует запрос по заголовку Authorization.
// Используется и REST-обвязкой, и MCP-сервером.
func AgentFromRequest(svc *core.Service, r *http.Request) (core.Agent, error) {
	h := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || strings.TrimSpace(token) == "" {
		return core.Agent{}, core.ErrUnauthorized
	}
	return svc.Authenticate(strings.TrimSpace(token))
}

func (s *Server) auth(next authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agent, err := AgentFromRequest(s.svc, r)
		if err != nil {
			writeError(w, err)
			return
		}
		next(w, r, agent)
	}
}

// --- Хендлеры ---

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	// Лимит берётся до разбора тела: иначе поток нечитаемых запросов на
	// единственную неаутентифицированную запись ничего не стоит.
	grant, err := s.limiter.AllowRegistration(s.limiter.ClientIP(r))
	if err != nil {
		writeError(w, err)
		return
	}
	var req struct {
		Name    string `json:"name"`
		Persona string `json:"persona"`
	}
	if !decode(w, r, &req) {
		return
	}
	agent, key, err := s.svc.RegisterAgent(req.Name, req.Persona)
	if err != nil {
		refundInvalid(&grant, err)
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"agent":   agent,
		"api_key": key,
		"note":    "Сохраните api_key: он показывается только один раз.",
	})
}

func (s *Server) handleMe(w http.ResponseWriter, _ *http.Request, agent core.Agent) {
	writeJSON(w, http.StatusOK, agent)
}

// --- Ключи агента ---
//
// Идентичность агента обязана переживать компрометацию секрета: протокол,
// голоса и вердикт ссылаются на стабильный agent_id, а ключи сменяемы.
// Порядок ротации — выпустить новый, затем отозвать старый.
// См. docs/adr/0005-credential-rotation-and-revocation.md.

// LogCredentialEvent записывает изменение набора секретов агента. Экспортирован
// для MCP-обвязки: событие обязано выглядеть одинаково на обоих транспортах.
//
// Адрес клиента здесь обязателен, а не декоративен. Сами по себе «выпущен» и
// «отозван» неотличимы для ротации владельцем и для угона украденным ключом —
// это буквально одна и та же пара операций. Различает их адрес: отзыв с
// адреса, с которого агент никогда не работал, — единственное механическое
// правило, по которому критерий отката ADR 0005 вообще разрешим.
//
// Строка пишется только после успеха, поэтому credentialID к этому моменту
// проверен хранилищем и не может быть произвольным текстом клиента.
func LogCredentialEvent(log *slog.Logger, event string, agent core.Agent, credentialID, clientIP string) {
	log.Info(event, "agent", agent.ID, "credential", credentialID, "ip", clientIP)
}

func (s *Server) handleIssueCredential(w http.ResponseWriter, r *http.Request, agent core.Agent) {
	grant, err := s.limiter.AllowCredentialIssue(agent.ID, s.limiter.ClientIP(r))
	if err != nil {
		writeError(w, err)
		return
	}
	credential, key, err := s.svc.IssueCredential(agent)
	if err != nil {
		refundInvalid(&grant, err)
		writeError(w, err)
		return
	}
	LogCredentialEvent(s.log, "выпущен ключ агента", agent, credential.ID, s.limiter.ClientIP(r))
	writeJSON(w, http.StatusCreated, map[string]any{
		"credential": credential,
		"api_key":    key,
		"note":       "Сохраните api_key: он показывается только один раз.",
	})
}

func (s *Server) handleListCredentials(w http.ResponseWriter, _ *http.Request, agent core.Agent) {
	list, err := s.svc.Credentials(agent)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": list})
}

func (s *Server) handleRevokeCredential(w http.ResponseWriter, r *http.Request, agent core.Agent) {
	credentialID := r.PathValue("id")
	if err := s.svc.RevokeCredential(agent, credentialID); err != nil {
		writeError(w, err)
		return
	}
	LogCredentialEvent(s.log, "отозван ключ агента", agent, credentialID, s.limiter.ClientIP(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCreateDebate(w http.ResponseWriter, r *http.Request, agent core.Agent) {
	var req struct {
		Question       string `json:"question"`
		Description    string `json:"description"`
		Stance         string `json:"stance"`
		Mode           string `json:"mode"`
		Rounds         int    `json:"rounds"`
		TurnTimeoutSec int    `json:"turn_timeout_sec"`
		PrepTimeSec    int    `json:"prep_time_sec"`
		Observer       bool   `json:"observer"`
	}
	grant, err := s.limiter.AllowDebateCreation(agent.ID, s.limiter.ClientIP(r))
	if err != nil {
		writeError(w, err)
		return
	}
	if !decode(w, r, &req) {
		return
	}
	v, err := s.svc.CreateDebate(agent, core.CreateDebateParams{
		Question:       req.Question,
		Description:    req.Description,
		Stance:         req.Stance,
		Mode:           core.DebateMode(req.Mode),
		Rounds:         req.Rounds,
		TurnTimeoutSec: req.TurnTimeoutSec,
		PrepTimeSec:    req.PrepTimeSec,
		Observer:       req.Observer,
	})
	if err != nil {
		refundInvalid(&grant, err)
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) handleDeleteDebate(w http.ResponseWriter, r *http.Request, agent core.Agent) {
	if err := s.svc.DeleteDebate(agent, r.PathValue("id")); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListDebates(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := s.svc.ListDebates(r.URL.Query().Get("status"), limit)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"debates": list})
}

func (s *Server) handleGetDebate(w http.ResponseWriter, r *http.Request) {
	v, err := s.svc.GetDebate(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	afterSeq, _ := strconv.ParseInt(r.URL.Query().Get("after_seq"), 10, 64)
	msgs, err := s.svc.Messages(r.PathValue("id"), afterSeq)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
}

// handleExport отдаёт дебаты как канонический версионированный JSONL-поток —
// тот же артефакт, что лежит в golden-трассах (docs/adr/0002-protocol-schema-v1.md,
// docs/adr/0006-debate-export-endpoint.md).
//
// Аутентификации нет намеренно: экспорт — это композиция двух уже публичных
// чтений, состояния и протокола, собранная из того же представления дебатов.
// Всё, что скрывает публичное чтение, скрывает и экспорт. Зато лимиты нужны
// оба: слот на адрес — за честность между клиентами, потолок на процесс — за
// память, потому что слот на адрес её не ограничивает.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	debateID := r.PathValue("id")
	clientIP := s.limiter.ClientIP(r)
	releaseAddress, ok := s.exportsByAddress.acquire(clientIP)
	if !ok {
		s.refuseExport(w, clientIP)
		return
	}
	defer releaseAddress()
	select {
	case s.exports <- struct{}{}:
		defer func() { <-s.exports }()
	default:
		// Отказ сразу, а не очередь: очередь на неаутентифицированном маршруте
		// — это тот же расход памяти, только отложенный.
		s.refuseExport(w, clientIP)
		return
	}

	readStarted := time.Now()
	snapshot, err := s.svc.ExportSnapshot(r.Context(), debateID)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// Клиент ушёл, пока запрос ждал очереди, — не событие оператора.
			// Но статус обязан быть явным: молчаливый возврат отдал бы неявный
			// 200 с пустым телом, то есть успешный экспорт из нуля записей.
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "экспорт прерван"})
			return
		}
		s.failExport(w, "экспорт: чтение дебатов", debateID, err)
		return
	}
	read := time.Since(readStarted)

	encodeStarted := time.Now()
	records, err := protocol.Stream(snapshot)
	if err != nil {
		s.failExport(w, "экспорт: сборка потока", debateID, err)
		return
	}
	// Артефакт кодируется целиком до первого записанного байта: потоковая
	// запись, отказавшая на середине, уже отдала бы 200 и префикс записей, а
	// оборванный JSONL неотличим от коротких дебатов. Объём ограничен лимитами
	// сервиса на участников, раунды и длину реплики.
	data, err := protocol.MarshalJSONL(records)
	if err != nil {
		s.failExport(w, "экспорт: сериализация потока", debateID, err)
		return
	}
	// Чтение и кодирование измеряются раздельно: чтение включает ожидание
	// замка переходов, и без разделения медленный чужой писатель выглядел бы
	// как разросшийся артефакт. Логируется только превышение порога, поэтому
	// поток запросов не может утопить сигнал отказа лимитера.
	if encode := time.Since(encodeStarted); read >= s.slowExport || encode >= s.slowExport {
		s.log.Warn("экспорт: медленная сборка", "debate", debateID,
			"read_ms", read.Milliseconds(), "encode_ms", encode.Milliseconds(), "bytes", len(data))
	}

	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", debateID+".jsonl"))
	// Артефакт содержит текст участников как есть: отдавать его на угадывание
	// типа браузеру незачем.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Длина объявляется, поэтому оборванная запись остаётся оборванной записью,
	// а не превращается в короткие, но правдоподобные дебаты.
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	// Предел на запись: слот потолка держится до её конца, и клиент, переставший
	// читать, иначе занимает его навсегда.
	//
	// Предел снимается после записи явно. Он ставится на соединение, а не на
	// запрос; текущий net/http снимает его сам в конце запроса, но единственная
	// граница, ограничивающая удержание слота, не должна зависеть от чужой
	// детали реализации: оставленный предел оборвал бы следующий ответ на том же
	// keep-alive соединении — чужой маршрут, чужой клиент, ни строки в логе.
	//
	// ResponseWriter без поддержки предела — не повод отказывать в ответе, но и
	// не мелочь: обёртка без Unwrap молча снимет единственную границу удержания.
	// Поэтому один раз на процесс это попадает в лог.
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(exportWriteTimeout)); err != nil {
		s.deadlineWarning.Do(func() {
			s.log.Error("экспорт: предел на запись недоступен, слот потолка удерживается без границы", "err", err)
		})
	}
	defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	w.WriteHeader(http.StatusOK)
	// Обрыв соединения клиентом — не событие оператора: writeJSON на остальных
	// маршрутах молчит по той же причине.
	_, _ = w.Write(data)
}

// refuseExport отвечает на исчерпанный потолок — общий или долю адреса.
// Логируется той же выборкой, что и отказы лимитера: иначе исчерпанный потолок
// выглядит как недоступность маршрута вообще, без следа на сервере, и критерию
// отката ADR 0006 стрелять не по чему.
func (s *Server) refuseExport(w http.ResponseWriter, clientIP string) {
	s.limiter.LogRefusal(ratelimit.ScopeExportCeiling, clientIP)
	w.Header().Set("Retry-After", strconv.Itoa(exportRetryAfterSec))
	writeJSON(w, http.StatusServiceUnavailable,
		map[string]string{"error": "экспорт занят, повторите запрос"})
}

// failExport логирует сбой экспорта и отвечает, не пересказывая клиенту
// внутреннюю ошибку. Логируется только то, что клиент не мог вызвать сам:
// отсутствующие дебаты и некорректный запрос — не события оператора, а поток
// 404 иначе стал бы способом писать в лог. Всё остальное означает, что дебаты
// не экспортируются ни одним запросом, а не «сейчас», — это сигнал отката
// (docs/adr/0006-debate-export-endpoint.md).
func (s *Server) failExport(w http.ResponseWriter, event, debateID string, err error) {
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, core.ErrValidation) {
		writeError(w, err)
		return
	}
	s.log.Error(event, "debate", debateID, "err", err)
	writeError(w, errors.New("экспорт дебатов недоступен"))
}

func (s *Server) handleJoin(w http.ResponseWriter, r *http.Request, agent core.Agent) {
	var req struct {
		Stance string `json:"stance"`
	}
	if r.ContentLength > 0 && !decode(w, r, &req) {
		return
	}
	v, err := s.svc.JoinDebate(agent, r.PathValue("id"), req.Stance)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request, agent core.Agent) {
	v, err := s.svc.StartDebate(agent, r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleTurn(w http.ResponseWriter, r *http.Request, agent core.Agent) {
	waitSec, _ := strconv.Atoi(r.URL.Query().Get("wait_sec"))
	waitSec = min(max(waitSec, 0), MaxWaitSec)
	st, err := s.svc.WaitTurn(r.Context(), agent, r.PathValue("id"), time.Duration(waitSec)*time.Second)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request, agent core.Agent) {
	var req struct {
		Text           string `json:"text"`
		SupportAgentID string `json:"support_agent_id"`
	}
	if !decode(w, r, &req) {
		return
	}
	msg, err := s.svc.PostArgument(r.Context(), agent, r.PathValue("id"), req.Text, req.SupportAgentID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, msg)
}

// handleEvents — SSE-поток: опционально реплей протокола с after_seq, затем live.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	debateID := r.PathValue("id")
	if _, err := s.svc.GetDebate(debateID); err != nil {
		writeError(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, errors.New("SSE не поддерживается"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Отправляем заголовки сразу: клиент видит, что подключение принято,
	// не дожидаясь первого события или heartbeat.
	flusher.Flush()

	// Подписка до реплея, чтобы не потерять события между ними.
	ch := s.svc.Subscribe(debateID)
	defer s.svc.Unsubscribe(debateID, ch)

	seen := int64(0)
	if v := r.URL.Query().Get("after_seq"); v != "" {
		afterSeq, _ := strconv.ParseInt(v, 10, 64)
		msgs, err := s.svc.Messages(debateID, afterSeq)
		if err != nil {
			s.log.Error("SSE replay: чтение протокола", "debate", debateID, "err", err)
			return
		}
		for _, m := range msgs {
			msg := m
			if err := writeSSE(w, core.Event{Type: core.EventMessage, DebateID: debateID,
				Round: m.Round, AgentID: m.SpeakerID, AgentName: m.SpeakerName, Message: &msg}); err != nil {
				s.logSSEError("replay", debateID, core.EventMessage, err)
				return
			}
			seen = m.Seq
		}
		flusher.Flush()
	}

	heartbeat := time.NewTicker(s.heartbeatInterval)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if err := writeSSEHeartbeat(w); err != nil {
				s.logSSEError("heartbeat", debateID, "heartbeat", err)
				return
			}
			flusher.Flush()
		case ev := <-ch:
			// Не дублируем сообщения, уже отданные реплеем.
			if ev.Message != nil && ev.Message.Seq <= seen {
				continue
			}
			if err := writeSSE(w, ev); err != nil {
				s.logSSEError("live", debateID, ev.Type, err)
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) logSSEError(scope, debateID, eventType string, err error) {
	var protocolErr *sseProtocolError
	if errors.As(err, &protocolErr) {
		s.log.Error("SSE protocol: отклонено событие", "scope", scope, "debate", debateID, "type", eventType, "err", err)
		return
	}
	s.log.Warn("SSE transport: запись события", "scope", scope, "debate", debateID, "type", eventType, "err", err)
}

type sseProtocolError struct{ err error }

func (e *sseProtocolError) Error() string { return "marshal SSE event: " + e.err.Error() }
func (e *sseProtocolError) Unwrap() error { return e.err }

type sseTransportError struct{ err error }

func (e *sseTransportError) Error() string { return "write SSE event: " + e.err.Error() }
func (e *sseTransportError) Unwrap() error { return e.err }

func writeSSE(w http.ResponseWriter, ev core.Event) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return &sseProtocolError{err: err}
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, data); err != nil {
		return &sseTransportError{err: err}
	}
	return nil
}

func writeSSEHeartbeat(w http.ResponseWriter) error {
	if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
		return &sseTransportError{err: err}
	}
	return nil
}

// --- Утилиты ---

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeError(w, fmt.Errorf("%w: некорректный JSON: %v", core.ErrValidation, err))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError отдаёт ошибку в общем для сервиса формате. Экспортирован для
// MCP-обвязки, которая отклоняет запрос до JSON-RPC-слоя.
func WriteError(w http.ResponseWriter, err error) { writeError(w, err) }

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, core.ErrValidation):
		status = http.StatusBadRequest
	case errors.Is(err, core.ErrUnauthorized):
		status = http.StatusUnauthorized
	case errors.Is(err, core.ErrForbidden):
		status = http.StatusForbidden
	case errors.Is(err, store.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, core.ErrNotYourTurn), errors.Is(err, core.ErrBadState),
		errors.Is(err, store.ErrLastCredential), errors.Is(err, store.ErrTooManyCredentials):
		status = http.StatusConflict
	case errors.Is(err, ratelimit.ErrLimited):
		status = http.StatusTooManyRequests
		// Заголовок ставится до WriteHeader внутри writeJSON.
		var limitErr *ratelimit.LimitError
		if errors.As(err, &limitErr) && limitErr.RetryAfterSeconds() > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(limitErr.RetryAfterSeconds()))
		}
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// Контекстные хелперы (используются MCP-обвязкой).

type ctxKey struct{}

type clientIPKey struct{}

// WithAgent кладёт аутентифицированного агента в контекст.
func WithAgent(ctx context.Context, a core.Agent) context.Context {
	return context.WithValue(ctx, ctxKey{}, a)
}

// AgentFrom достаёт агента из контекста.
func AgentFrom(ctx context.Context) (core.Agent, bool) {
	a, ok := ctx.Value(ctxKey{}).(core.Agent)
	return a, ok
}

// WithClientIP кладёт в контекст адрес клиента, разрешённый лимитером.
// У MCP-инструментов нет http.Request, а ключ лимита им нужен.
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPKey{}, ip)
}

// ClientIPFrom достаёт адрес клиента из контекста.
func ClientIPFrom(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPKey{}).(string)
	return ip
}
