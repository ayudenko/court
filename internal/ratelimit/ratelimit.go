// Package ratelimit bounds abuse of the public HTTP surface: registration
// floods, debate-creation floods, and accumulation of long-lived streams.
//
// Limits are always per client identity. A global limit would turn one abusive
// client into an outage for everyone, which is the failure these limits exist
// to prevent. Every limit is disabled when its configured value is zero, so
// tests and local runs keep the unlimited behaviour.
//
// See docs/adr/0003-http-rate-limiting.md.
package ratelimit

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// ErrLimited matches every rejection produced by this package. REST maps it to
// 429; MCP returns it to the calling agent as a tool error.
var ErrLimited = errors.New("превышен лимит запросов")

// defaultMaxTrackedClients bounds memory. The key space is attacker-controlled,
// so an unbounded table of limiters is itself an exhaustion vector.
const defaultMaxTrackedClients = 10000

// maxKeyLen bounds a key that is not a valid address. Entry count alone does not
// bound bytes: a client-settable header can be as large as the header budget.
const maxKeyLen = 64

// ipv6GroupBits charges IPv6 traffic per /64. Hosts are routinely delegated a
// /64 or larger, so charging a full /128 would let one machine mint unlimited
// fresh buckets and defeat every address-keyed limit. IPv4 is charged per
// address.
const ipv6GroupBits = 64

// Rejection logging is sampled per scope: the first few rejections of an
// incident are shown in full, then one line every logSampleInterval carrying
// the number suppressed since the last one.
const (
	logSampleBurst    = 5
	logSampleInterval = 10 * time.Second
)

// Scope names the limit that rejected a request. It appears in the client-facing
// error and in the rejection log, so both a caller and an operator can tell
// which budget was exhausted — in particular whether a rejection is the
// caller's own doing or the shared address budget, which is the difference
// between a client bug and a reason to raise the limits.
const (
	ScopeRegistration      = "регистрация агентов с этого адреса"
	ScopeDebateByAgent     = "создание дебатов этим агентом"
	ScopeDebateByIP        = "создание дебатов с этого адреса"
	ScopeCredentialByAgent = "выпуск ключей этим агентом"
	ScopeCredentialByIP    = "выпуск ключей с этого адреса"
	ScopeStreamByAgent     = "одновременные подключения этого агента"
	ScopeStreamByIP        = "одновременные подключения с этого адреса"
	// ScopeExportCeiling — потолок одновременных сборок экспорта. Сам потолок
	// живёт в обвязке HTTP: он ограничивает память процесса, а не клиента, и
	// ключа у него нет. Но сигнал обязан быть тем же и с той же выборкой, иначе
	// поток отказов утопит остальные.
	ScopeExportCeiling = "одновременные сборки экспорта"
)

// Config holds the operator-visible limits. Its zero value disables everything.
type Config struct {
	// RegistrationsPerHourPerIP limits agent registration, the only
	// unauthenticated write in the service.
	RegistrationsPerHourPerIP int
	// DebatesPerHourPerAgent limits debate creation, which is what actually
	// spends the service owner's moderator key.
	DebatesPerHourPerAgent int
	// DebatesPerHourPerIP bounds the same operation by address. Agent identities
	// are permanent, so an agent-keyed budget alone grows with the number of
	// agents a caller has accumulated over time; only the address bound keeps
	// total debate creation — and therefore total moderator spend — finite.
	DebatesPerHourPerIP int
	// CredentialsPerHourPerAgent limits credential issuance, an authenticated
	// write that creates durable rows. The active-credential cap in the core
	// bounds how many secrets work at once; this bounds how fast revoked rows
	// accumulate.
	CredentialsPerHourPerAgent int
	// CredentialsPerHourPerIP bounds the same operation by address, for the
	// reason DebatesPerHourPerIP exists: an agent-keyed budget alone grows with
	// the number of agents one caller has accumulated.
	CredentialsPerHourPerIP int
	// StreamsPerClient caps concurrent long-poll, SSE, and /mcp requests. It is
	// applied to the agent and to the address independently: an agent-only cap
	// would let one address add another StreamsPerClient slots per registered
	// agent and still reach the fronting proxy's connection limit from a single
	// host, for the same reason debate creation needs an address bound.
	StreamsPerClient int
	// ClientIPHeader names the header a trusted reverse proxy sets to the real
	// client address (Fly-Client-IP on Fly.io). Empty means RemoteAddr, which
	// is correct only for direct connections: behind a proxy every request
	// would otherwise share one bucket. Setting it without such a proxy lets a
	// client choose its own bucket and bypass address-keyed limits.
	ClientIPHeader string
	// MaxTrackedClients bounds each bucket table; zero selects the default.
	MaxTrackedClients int
}

// LimitError reports which limit rejected a request. RetryAfter is zero for the
// concurrency limit, where the wait depends on other connections closing rather
// than on elapsed time.
type LimitError struct {
	Scope      string
	RetryAfter time.Duration
}

func (e *LimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("%s (%s): повторите через %d с", ErrLimited.Error(), e.Scope, e.RetryAfterSeconds())
	}
	return fmt.Sprintf("%s (%s)", ErrLimited.Error(), e.Scope)
}

func (e *LimitError) Unwrap() error { return ErrLimited }

// RetryAfterSeconds rounds up to whole seconds, the only unit the Retry-After
// header accepts; a sub-second wait must still advertise at least one second.
func (e *LimitError) RetryAfterSeconds() int {
	if e.RetryAfter <= 0 {
		return 0
	}
	return int(math.Ceil(e.RetryAfter.Seconds()))
}

// Grant is the accounting handle of an allowed request. Refund returns the
// spent tokens when the request turns out to have been invalid, so a client
// sending malformed payloads cannot lock itself out of a working operation.
// A zero Grant refunds nothing.
type Grant struct {
	refunds []func()
}

// Refund returns every token this grant spent. Calling it more than once is
// safe; calling it after the operation succeeded would hand back a real
// allowance, so only failure paths may call it.
func (g *Grant) Refund() {
	for _, refund := range g.refunds {
		refund()
	}
	g.refunds = nil
}

// Limiter enforces the configured limits. A nil *Limiter enforces nothing, so
// callers that have no limiter wired do not need nil checks at every call site.
type Limiter struct {
	clientIPHeader string
	now            func() time.Time
	log            *slog.Logger
	logSampler     *logSampler
	registrations  *bucketTable
	debatesByAgent *bucketTable
	debatesByIP    *bucketTable
	credsByAgent   *bucketTable
	credsByIP      *bucketTable
	streams        *streamTable
}

// Option customizes a Limiter.
type Option func(*Limiter)

// WithClock replaces the time source so bucket refill is deterministic in tests.
func WithClock(now func() time.Time) Option {
	return func(l *Limiter) { l.now = now }
}

// WithLogger records every rejection. Without observable rejections an operator
// cannot tell an attack from a misconfiguration, and the ADR's rollback
// criterion has nothing to fire on.
func WithLogger(log *slog.Logger) Option {
	return func(l *Limiter) { l.log = log }
}

// New builds a limiter from cfg.
func New(cfg Config, options ...Option) *Limiter {
	maxTracked := cfg.MaxTrackedClients
	if maxTracked <= 0 {
		maxTracked = defaultMaxTrackedClients
	}
	limiter := &Limiter{
		clientIPHeader: cfg.ClientIPHeader,
		now:            time.Now,
		logSampler:     newLogSampler(),
		registrations:  newBucketTable(ScopeRegistration, cfg.RegistrationsPerHourPerIP, maxTracked),
		debatesByAgent: newBucketTable(ScopeDebateByAgent, cfg.DebatesPerHourPerAgent, maxTracked),
		debatesByIP:    newBucketTable(ScopeDebateByIP, cfg.DebatesPerHourPerIP, maxTracked),
		credsByAgent:   newBucketTable(ScopeCredentialByAgent, cfg.CredentialsPerHourPerAgent, maxTracked),
		credsByIP:      newBucketTable(ScopeCredentialByIP, cfg.CredentialsPerHourPerIP, maxTracked),
		streams:        newStreamTable(cfg.StreamsPerClient),
	}
	for _, option := range options {
		option(limiter)
	}
	return limiter
}

// AllowRegistration accounts one agent registration from clientIP.
func (l *Limiter) AllowRegistration(clientIP string) (Grant, error) {
	if l == nil {
		return Grant{}, nil
	}
	return l.spend(map[string]string{"ip": clientIP}, bucketFor(l.registrations, clientIP))
}

// AllowDebateCreation accounts one debate against both the creating agent and
// the address it came from. The agent key is the stable agent rather than the
// credential, so a second credential under issue #5 will not double the budget;
// the address key is what keeps the total finite as agents accumulate.
func (l *Limiter) AllowDebateCreation(agentID, clientIP string) (Grant, error) {
	if l == nil {
		return Grant{}, nil
	}
	return l.spend(map[string]string{"agent": agentID, "ip": clientIP},
		bucketFor(l.debatesByAgent, agentID), bucketFor(l.debatesByIP, clientIP))
}

// AllowCredentialIssue accounts one credential issued by agentID from clientIP.
// The agent key is the stable agent rather than the presenting credential, so
// rotating or holding several keys never multiplies the budget.
func (l *Limiter) AllowCredentialIssue(agentID, clientIP string) (Grant, error) {
	if l == nil {
		return Grant{}, nil
	}
	return l.spend(map[string]string{"agent": agentID, "ip": clientIP},
		bucketFor(l.credsByAgent, agentID), bucketFor(l.credsByIP, clientIP))
}

// spend charges every bucket or none: a request refused by a later bucket must
// not leave earlier ones debited, or repeated rejections would drain a budget
// the caller never got to use.
func (l *Limiter) spend(logKeys map[string]string, charges ...charge) (Grant, error) {
	now := l.now()
	var grant Grant
	for _, c := range charges {
		refund, err := c.table.allow(c.key, now)
		if err != nil {
			grant.Refund()
			l.logRejection(err, logKeys)
			return Grant{}, err
		}
		if refund != nil {
			grant.refunds = append(grant.refunds, refund)
		}
	}
	return grant, nil
}

type charge struct {
	table *bucketTable
	key   string
}

func bucketFor(table *bucketTable, key string) charge { return charge{table: table, key: key} }

// AcquireStream reserves a concurrent stream slot for a request's client. An
// authenticated caller is charged to its agent and to its address; pass an
// empty agentID for an unauthenticated one. The returned release must be called
// when the stream ends; calling it more than once is safe. On rejection release
// is a no-op, so `defer release()` is always valid.
func (l *Limiter) AcquireStream(agentID, clientIP string) (release func(), err error) {
	if l == nil {
		return func() {}, nil
	}
	held := make([]func(), 0, 2)
	releaseHeld := func() {
		for _, r := range held {
			r()
		}
	}
	slots := []struct {
		scope string
		key   string
	}{{ScopeStreamByIP, "ip:" + clientIP}}
	if agentID != "" {
		slots = append(slots, struct {
			scope string
			key   string
		}{ScopeStreamByAgent, "agent:" + agentID})
	}
	for _, slot := range slots {
		release, err := l.streams.acquire(slot.key, slot.scope)
		if err != nil {
			// All or nothing: a slot held for a request that was refused anyway
			// would stay held for the whole request and shrink the real cap.
			releaseHeld()
			l.logRejection(err, map[string]string{"agent": agentID, "ip": clientIP})
			return func() {}, err
		}
		held = append(held, release)
	}
	var once sync.Once
	return func() { once.Do(releaseHeld) }, nil
}

// LogRefusal records a refusal that some other component issued but that
// belongs to the same abuse signal. The transport owns limits whose key is
// neither an agent nor an address — a ceiling on the process — yet an operator
// reads one signal, so those refusals go through the same message and the same
// per-scope sampling. Without it a saturated ceiling is an outage with no
// server-side evidence at all.
func (l *Limiter) LogRefusal(scope, clientIP string) {
	if l == nil || l.log == nil {
		return
	}
	suppressed, ok := l.logSampler.admit(scope, l.now())
	if !ok {
		return
	}
	attrs := []any{"scope", scope}
	if clientIP != "" {
		attrs = append(attrs, "ip", clientIP)
	}
	if suppressed > 0 {
		attrs = append(attrs, "suppressed_since_last", suppressed)
	}
	l.log.Warn("лимит: запрос отклонён", attrs...)
}

// logRejection records a rejection, sampled per scope. Rejections happen
// precisely when a client is flooding, so an unsampled line per request would
// let an attacker drown the very signal the rollback criterion reads — and pay
// for it with a serialized write per request. Suppressed rejections are counted
// and reported with the next line that gets through.
func (l *Limiter) logRejection(err error, keys map[string]string) {
	if l == nil || l.log == nil {
		return
	}
	scope := ""
	var limitErr *LimitError
	if errors.As(err, &limitErr) {
		scope = limitErr.Scope
	}
	suppressed, ok := l.logSampler.admit(scope, l.now())
	if !ok {
		return
	}
	attrs := make([]any, 0, 2*len(keys)+4)
	attrs = append(attrs, "scope", scope)
	for name, value := range keys {
		if value != "" {
			attrs = append(attrs, name, value)
		}
	}
	if suppressed > 0 {
		attrs = append(attrs, "suppressed_since_last", suppressed)
	}
	l.log.Warn("лимит: запрос отклонён", attrs...)
}

// logSampler admits a bounded number of log lines per scope and counts the rest.
type logSampler struct {
	mu       sync.Mutex
	limiters map[string]*rate.Limiter
	dropped  map[string]int
}

func newLogSampler() *logSampler {
	return &logSampler{limiters: map[string]*rate.Limiter{}, dropped: map[string]int{}}
}

func (s *logSampler) admit(scope string, now time.Time) (suppressed int, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	limiter := s.limiters[scope]
	if limiter == nil {
		// A short burst so the first rejections of an incident are all visible,
		// then a trickle that keeps the scope present in the log without flooding.
		limiter = rate.NewLimiter(rate.Every(logSampleInterval), logSampleBurst)
		s.limiters[scope] = limiter
	}
	if !limiter.AllowN(now, 1) {
		s.dropped[scope]++
		return 0, false
	}
	suppressed = s.dropped[scope]
	delete(s.dropped, scope)
	return suppressed, true
}

// ClientIP resolves the address a limit is charged to.
//
// Only one trusted hop is supported. The proxy's own value is taken from the
// last header line, then the last comma-separated field of it: an intermediary
// either appends to the existing line or adds a new one after the client's, so
// in both shapes anything the client injected stays to the left. Behind two
// trusted proxies the result is the inner proxy's address, which collapses every
// client into one bucket — deploy court behind a single hop.
//
// A value that is not an address is discarded rather than trusted, so a
// misconfigured header cannot turn the key space into arbitrary strings.
func (l *Limiter) ClientIP(r *http.Request) string {
	header := ""
	if l != nil {
		header = l.clientIPHeader
	}
	if header != "" {
		if lines := r.Header.Values(header); len(lines) > 0 {
			fields := strings.Split(lines[len(lines)-1], ",")
			if key, ok := addressKey(fields[len(fields)-1]); ok {
				return key
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if key, ok := addressKey(host); ok {
		return key
	}
	// RemoteAddr is not client-controlled, but it is not always an address
	// either (a unix socket, or a test fixture). Bound it rather than trust it.
	if len(host) > maxKeyLen {
		return host[:maxKeyLen]
	}
	return host
}

// addressKey normalizes an address into the identity a limit is charged to.
func addressKey(value string) (string, bool) {
	address, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil {
		return "", false
	}
	address = address.Unmap()
	if !address.Is6() {
		return address.String(), true
	}
	prefix, err := address.Prefix(ipv6GroupBits)
	if err != nil {
		return "", false
	}
	return prefix.String(), true
}

// --- Token buckets ---

// bucketTable holds one token bucket per key. A nil table enforces nothing.
type bucketTable struct {
	scope string
	limit rate.Limit
	burst int
	max   int

	mu      sync.Mutex
	entries map[string]*bucketEntry
}

type bucketEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func newBucketTable(scope string, perHour, max int) *bucketTable {
	if perHour <= 0 {
		return nil
	}
	return &bucketTable{
		scope: scope,
		limit: rate.Every(time.Hour / time.Duration(perHour)),
		// Burst equals the hourly allowance: a client acting once is never
		// delayed, while sustained use converges to the configured rate.
		burst:   perHour,
		max:     max,
		entries: make(map[string]*bucketEntry),
	}
}

// allow charges one token and returns the refund that gives it back.
func (t *bucketTable) allow(key string, now time.Time) (refund func(), err error) {
	if t == nil {
		return nil, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	entry := t.entries[key]
	if entry == nil {
		t.evictLocked(now)
		entry = &bucketEntry{limiter: rate.NewLimiter(t.limit, t.burst)}
		t.entries[key] = entry
	}
	entry.lastSeen = now
	reservation := entry.limiter.ReserveN(now, 1)
	if !reservation.OK() {
		return nil, &LimitError{Scope: t.scope}
	}
	if delay := reservation.DelayFrom(now); delay > 0 {
		// The caller is rejected rather than queued, so give the token back.
		reservation.CancelAt(now)
		return nil, &LimitError{Scope: t.scope, RetryAfter: delay}
	}
	var once sync.Once
	return func() { once.Do(func() { reservation.CancelAt(now) }) }, nil
}

// evictLocked keeps the table bounded. A fully refilled bucket is
// indistinguishable from a fresh one, so dropping it loses no enforcement;
// those go first. Only if that is not enough are the least recently seen
// entries removed, down to 90% of the cap so the scan is amortized.
func (t *bucketTable) evictLocked(now time.Time) {
	if len(t.entries) < t.max {
		return
	}
	for key, entry := range t.entries {
		if entry.limiter.TokensAt(now) >= float64(t.burst) {
			delete(t.entries, key)
		}
	}
	target := t.max * 9 / 10
	if len(t.entries) <= target {
		return
	}
	stale := make([]string, 0, len(t.entries))
	for key := range t.entries {
		stale = append(stale, key)
	}
	sort.Slice(stale, func(i, j int) bool {
		return t.entries[stale[i]].lastSeen.Before(t.entries[stale[j]].lastSeen)
	})
	for _, key := range stale[:len(t.entries)-target] {
		delete(t.entries, key)
	}
}

// --- Concurrent streams ---

// streamTable counts open long-lived connections per key. A nil table enforces
// nothing. Entries exist only while a connection is open, so the table is
// bounded by however many connections the fronting proxy admits.
type streamTable struct {
	max int

	mu   sync.Mutex
	open map[string]int
}

func newStreamTable(max int) *streamTable {
	if max <= 0 {
		return nil
	}
	return &streamTable{max: max, open: make(map[string]int)}
}

func (t *streamTable) acquire(key, scope string) (func(), error) {
	if t == nil {
		return func() {}, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.open[key] >= t.max {
		return func() {}, &LimitError{Scope: scope}
	}
	t.open[key]++
	var once sync.Once
	return func() {
		once.Do(func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			if t.open[key] <= 1 {
				delete(t.open, key)
				return
			}
			t.open[key]--
		})
	}, nil
}
