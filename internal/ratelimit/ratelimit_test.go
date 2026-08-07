package ratelimit

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClientIPTrustsTheConfiguredHeaderAndNothingElse(t *testing.T) {
	cases := []struct {
		name       string
		header     string
		sent       map[string]string
		added      []string
		remoteAddr string
		want       string
	}{{
		name:       "no configured header ignores whatever the client claims",
		sent:       map[string]string{"X-Forwarded-For": "1.2.3.4", "Fly-Client-IP": "1.2.3.4"},
		remoteAddr: "198.51.100.9:4321",
		want:       "198.51.100.9",
	}, {
		name:       "configured header wins",
		header:     "Fly-Client-IP",
		sent:       map[string]string{"Fly-Client-IP": "203.0.113.5"},
		remoteAddr: "10.0.0.1:1111",
		want:       "203.0.113.5",
	}, {
		// Intermediaries append, so the value the closest trusted proxy
		// observed is last; anything the client injected stays to its left.
		name:       "rightmost entry of an appended chain",
		header:     "X-Forwarded-For",
		sent:       map[string]string{"X-Forwarded-For": "9.9.9.9, 203.0.113.5"},
		remoteAddr: "10.0.0.1:1111",
		want:       "203.0.113.5",
	}, {
		name:       "missing configured header falls back to the connection",
		header:     "Fly-Client-IP",
		remoteAddr: "198.51.100.9:4321",
		want:       "198.51.100.9",
	}, {
		// http.Header.Get would return the first line, so a proxy that adds its
		// own line (HAProxy's `option forwardfor`) after the client's would let
		// the client keep the key.
		name:       "last header line wins over an injected earlier one",
		header:     "X-Forwarded-For",
		added:      []string{"9.9.9.9", "203.0.113.5"},
		remoteAddr: "10.0.0.1:1111",
		want:       "203.0.113.5",
	}, {
		name:       "a header value that is not an address is discarded",
		header:     "Fly-Client-IP",
		sent:       map[string]string{"Fly-Client-IP": strings.Repeat("junk", 5000)},
		remoteAddr: "198.51.100.9:4321",
		want:       "198.51.100.9",
	}, {
		// A host is routinely delegated a whole /64, so charging the exact
		// address would hand one machine unlimited fresh buckets.
		name:       "IPv6 is charged per /64",
		header:     "Fly-Client-IP",
		sent:       map[string]string{"Fly-Client-IP": "2001:db8:1:2:dead:beef:1:2"},
		remoteAddr: "10.0.0.1:1111",
		want:       "2001:db8:1:2::/64",
	}, {
		name:       "IPv4-mapped IPv6 stays a single address",
		header:     "Fly-Client-IP",
		sent:       map[string]string{"Fly-Client-IP": "::ffff:203.0.113.5"},
		remoteAddr: "10.0.0.1:1111",
		want:       "203.0.113.5",
	}, {
		name:       "unparsable RemoteAddr is used verbatim",
		remoteAddr: "unix-socket",
		want:       "unix-socket",
	}}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.RemoteAddr = testCase.remoteAddr
			for name, value := range testCase.sent {
				request.Header.Set(name, value)
			}
			for _, value := range testCase.added {
				request.Header.Add(testCase.header, value)
			}
			got := New(Config{ClientIPHeader: testCase.header}).ClientIP(request)
			if got != testCase.want {
				t.Fatalf("ClientIP = %q, want %q", got, testCase.want)
			}
			if len(got) > maxKeyLen {
				t.Fatalf("key length = %d, want at most %d", len(got), maxKeyLen)
			}
		})
	}
}

// TestAddressesInOneIPv6PrefixShareABucket is the load-bearing case for every
// address-keyed limit: without prefix grouping one host with a routed /64 gets
// an unlimited supply of fresh buckets.
func TestAddressesInOneIPv6PrefixShareABucket(t *testing.T) {
	limiter := New(Config{RegistrationsPerHourPerIP: 1, ClientIPHeader: "Fly-Client-IP"})

	first := clientIPOf(limiter, "2001:db8:abcd:1::1")
	second := clientIPOf(limiter, "2001:db8:abcd:1:ffff:ffff:ffff:ffff")
	if first != second {
		t.Fatalf("addresses in one /64 resolved to %q and %q, want one key", first, second)
	}
	if _, err := limiter.AllowRegistration(first); err != nil {
		t.Fatalf("first registration: %v", err)
	}
	if _, err := limiter.AllowRegistration(second); !errors.Is(err, ErrLimited) {
		t.Fatalf("second address in the same /64: %v, want ErrLimited", err)
	}
	// A different /64 is a different customer.
	other := clientIPOf(limiter, "2001:db8:abcd:2::1")
	if _, err := limiter.AllowRegistration(other); err != nil {
		t.Fatalf("different /64: %v", err)
	}
}

// TestDebateCreationIsBoundedByAddressAsWellAsAgent: agent identities are
// permanent, so an agent-only budget grows with however many agents a caller
// has accumulated.
func TestDebateCreationIsBoundedByAddressAsWellAsAgent(t *testing.T) {
	limiter := New(Config{DebatesPerHourPerAgent: 5, DebatesPerHourPerIP: 2})

	for i := range 2 {
		if _, err := limiter.AllowDebateCreation(fmt.Sprintf("agt_%d", i), "203.0.113.7"); err != nil {
			t.Fatalf("debate %d from a fresh agent: %v", i+1, err)
		}
	}
	if _, err := limiter.AllowDebateCreation("agt_third", "203.0.113.7"); !errors.Is(err, ErrLimited) {
		t.Fatalf("third agent behind the same address: %v, want ErrLimited", err)
	}
	if _, err := limiter.AllowDebateCreation("agt_third", "198.51.100.4"); err != nil {
		t.Fatalf("same agent from another address: %v", err)
	}
}

// TestAddressRejectionDoesNotChargeTheAgentBucket: a partially charged request
// would drain a budget the caller never got to use.
func TestAddressRejectionDoesNotChargeTheAgentBucket(t *testing.T) {
	limiter := New(Config{DebatesPerHourPerAgent: 2, DebatesPerHourPerIP: 1})

	if _, err := limiter.AllowDebateCreation("agt_1", "203.0.113.7"); err != nil {
		t.Fatalf("first debate: %v", err)
	}
	for range 5 {
		if _, err := limiter.AllowDebateCreation("agt_1", "203.0.113.7"); !errors.Is(err, ErrLimited) {
			t.Fatalf("expected the address bucket to reject: %v", err)
		}
	}
	// The agent spent one token, not six.
	if _, err := limiter.AllowDebateCreation("agt_1", "198.51.100.4"); err != nil {
		t.Fatalf("agent budget was drained by address-level rejections: %v", err)
	}
}

// TestRefundReturnsTheTokensOfAnInvalidRequest: a client that sends malformed
// arguments creates nothing, so charging it would let it lock itself out.
func TestRefundReturnsTheTokensOfAnInvalidRequest(t *testing.T) {
	limiter := New(Config{DebatesPerHourPerAgent: 1, DebatesPerHourPerIP: 1})

	for range 5 {
		grant, err := limiter.AllowDebateCreation("agt_1", "203.0.113.7")
		if err != nil {
			t.Fatalf("refunded budget was not restored: %v", err)
		}
		grant.Refund()
		// A second refund must not mint an extra token.
		grant.Refund()
	}
	if _, err := limiter.AllowDebateCreation("agt_1", "203.0.113.7"); err != nil {
		t.Fatalf("budget after only refunded attempts: %v", err)
	}
	if _, err := limiter.AllowDebateCreation("agt_1", "203.0.113.7"); !errors.Is(err, ErrLimited) {
		t.Fatalf("refunds inflated the allowance: %v, want ErrLimited", err)
	}
}

func TestRejectionsAreLogged(t *testing.T) {
	var logs bytes.Buffer
	limiter := New(
		Config{RegistrationsPerHourPerIP: 1, StreamsPerClient: 1},
		WithLogger(slog.New(slog.NewTextHandler(&logs, nil))),
	)

	if _, err := limiter.AllowRegistration("203.0.113.7"); err != nil {
		t.Fatalf("first registration: %v", err)
	}
	if logs.Len() != 0 {
		t.Fatalf("an allowed request was logged: %s", logs.String())
	}
	if _, err := limiter.AllowRegistration("203.0.113.7"); err == nil {
		t.Fatal("second registration was allowed")
	}
	if !strings.Contains(logs.String(), ScopeRegistration) || !strings.Contains(logs.String(), "203.0.113.7") {
		t.Fatalf("rejection log lacks the scope or the key an operator needs: %s", logs.String())
	}

	logs.Reset()
	release, _ := limiter.AcquireStream("", "203.0.113.7")
	defer release()
	if _, err := limiter.AcquireStream("", "203.0.113.7"); err == nil {
		t.Fatal("second stream was granted")
	}
	if !strings.Contains(logs.String(), ScopeStreamByIP) {
		t.Fatalf("stream rejection was not logged: %s", logs.String())
	}
}

func clientIPOf(limiter *Limiter, headerValue string) string {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "10.0.0.1:1111"
	request.Header.Set("Fly-Client-IP", headerValue)
	return limiter.ClientIP(request)
}

func TestBucketRefillsAtTheConfiguredRate(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	limiter := New(Config{RegistrationsPerHourPerIP: 2}, WithClock(func() time.Time { return now }))

	for i := range 2 {
		if _, err := limiter.AllowRegistration("client"); err != nil {
			t.Fatalf("request %d within the allowance: %v", i+1, err)
		}
	}

	_, err := limiter.AllowRegistration("client")
	var limitErr *LimitError
	if !errors.As(err, &limitErr) || !errors.Is(err, ErrLimited) {
		t.Fatalf("exhausted bucket returned %v, want a *LimitError matching ErrLimited", err)
	}
	// Two per hour means one token every 30 minutes.
	if got := limitErr.RetryAfterSeconds(); got < 1750 || got > 1810 {
		t.Fatalf("RetryAfterSeconds = %d, want about 1800", got)
	}

	now = now.Add(31 * time.Minute)
	if _, err := limiter.AllowRegistration("client"); err != nil {
		t.Fatalf("after the bucket refilled: %v", err)
	}
	// The refused request must not have consumed a token, or every rejection
	// would push the next success further away.
	if _, err := limiter.AllowRegistration("client"); err == nil {
		t.Fatal("bucket allowed two requests after a single refill; the rejected reservation was not cancelled")
	}
}

func TestZeroConfigAndNilLimiterEnforceNothing(t *testing.T) {
	for name, limiter := range map[string]*Limiter{"zero config": New(Config{}), "nil limiter": nil} {
		t.Run(name, func(t *testing.T) {
			for range 100 {
				if _, err := limiter.AllowRegistration("client"); err != nil {
					t.Fatalf("AllowRegistration: %v", err)
				}
				if _, err := limiter.AllowDebateCreation("agt_1", "client"); err != nil {
					t.Fatalf("AllowDebateCreation: %v", err)
				}
				release, err := limiter.AcquireStream("", "203.0.113.7")
				if err != nil {
					t.Fatalf("AcquireStream: %v", err)
				}
				defer release()
			}
		})
	}
}

// TestTrackedClientsStayBounded: the key space is attacker-controlled, so an
// unbounded table of limiters would itself be the exhaustion vector.
func TestTrackedClientsStayBounded(t *testing.T) {
	const max = 100
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	limiter := New(
		Config{RegistrationsPerHourPerIP: 1, MaxTrackedClients: max},
		WithClock(func() time.Time { return now }),
	)

	// Every key spends its only token, so no entry is refilled and eviction
	// cannot take the cheap path.
	for i := range max * 5 {
		if _, err := limiter.AllowRegistration(fmt.Sprintf("client-%d", i)); err != nil {
			t.Fatalf("first request from a fresh key %d: %v", i, err)
		}
	}
	if tracked := limiter.registrations.size(); tracked > max {
		t.Fatalf("tracked clients = %d, want at most %d", tracked, max)
	}

	// A key that is still tracked must keep its exhausted bucket.
	if _, err := limiter.AllowRegistration(fmt.Sprintf("client-%d", max*5-1)); err == nil {
		t.Fatal("the most recently seen key lost its spent token")
	}
}

func TestRefilledEntriesAreReclaimedBeforeActiveOnes(t *testing.T) {
	const max = 10
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	limiter := New(
		Config{RegistrationsPerHourPerIP: 1, MaxTrackedClients: max},
		WithClock(func() time.Time { return now }),
	)

	for i := range max {
		if _, err := limiter.AllowRegistration(fmt.Sprintf("old-%d", i)); err != nil {
			t.Fatalf("seeding key %d: %v", i, err)
		}
	}
	// An hour later every seeded bucket is full again, so all of them are
	// indistinguishable from fresh entries and may be dropped for free.
	now = now.Add(2 * time.Hour)
	if _, err := limiter.AllowRegistration("newcomer"); err != nil {
		t.Fatalf("newcomer: %v", err)
	}
	if tracked := limiter.registrations.size(); tracked != 1 {
		t.Fatalf("tracked clients = %d, want only the newcomer", tracked)
	}
}

func TestStreamSlotsAreReturnedExactlyOnce(t *testing.T) {
	limiter := New(Config{StreamsPerClient: 1})

	release, err := limiter.AcquireStream("", "203.0.113.7")
	if err != nil {
		t.Fatalf("first slot: %v", err)
	}
	if _, err := limiter.AcquireStream("", "203.0.113.7"); !errors.Is(err, ErrLimited) {
		t.Fatalf("second concurrent slot: %v, want ErrLimited", err)
	}

	release()
	// A double release must not create a slot out of nothing.
	release()

	first, err := limiter.AcquireStream("", "203.0.113.7")
	if err != nil {
		t.Fatalf("slot after release: %v", err)
	}
	defer first()
	if _, err := limiter.AcquireStream("", "203.0.113.7"); !errors.Is(err, ErrLimited) {
		t.Fatalf("double release inflated the allowance: %v, want ErrLimited", err)
	}
}

func TestRejectedStreamReleaseIsSafeToCall(t *testing.T) {
	limiter := New(Config{StreamsPerClient: 1})
	held, err := limiter.AcquireStream("", "203.0.113.7")
	if err != nil {
		t.Fatalf("first slot: %v", err)
	}
	defer held()

	// Callers defer release before checking the error, so a rejection must
	// still return a usable no-op that does not free somebody else's slot.
	release, err := limiter.AcquireStream("", "203.0.113.7")
	if err == nil {
		t.Fatal("second slot was granted")
	}
	release()
	if _, err := limiter.AcquireStream("", "203.0.113.7"); !errors.Is(err, ErrLimited) {
		t.Fatalf("releasing a rejected acquisition freed a held slot: %v", err)
	}
}

func TestConcurrentUseIsRaceFree(t *testing.T) {
	limiter := New(Config{RegistrationsPerHourPerIP: 50, DebatesPerHourPerAgent: 50, StreamsPerClient: 5})
	var wait sync.WaitGroup
	for worker := range 20 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			key := fmt.Sprintf("client-%d", worker%3)
			for range 50 {
				_, _ = limiter.AllowRegistration(key)
				_, _ = limiter.AllowDebateCreation(key, key)
				release, _ := limiter.AcquireStream("", key)
				release()
			}
		}()
	}
	wait.Wait()
}

func (t *bucketTable) size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.entries)
}

// TestStreamsAreBoundedByAddressAcrossAgents mirrors the debate limit: agent
// identities are permanent, so an agent-only cap lets one host add another full
// allowance per registered agent and still reach the proxy's connection limit.
func TestStreamsAreBoundedByAddressAcrossAgents(t *testing.T) {
	limiter := New(Config{StreamsPerClient: 2})
	const clientIP = "203.0.113.7"

	first, err := limiter.AcquireStream("agt_1", clientIP)
	if err != nil {
		t.Fatalf("first agent's slot: %v", err)
	}
	defer first()
	second, err := limiter.AcquireStream("agt_2", clientIP)
	if err != nil {
		t.Fatalf("second agent's slot: %v", err)
	}
	defer second()

	// A third agent behind the same address finds the address budget spent.
	_, err = limiter.AcquireStream("agt_3", clientIP)
	var limitErr *LimitError
	if !errors.As(err, &limitErr) || limitErr.Scope != ScopeStreamByIP {
		t.Fatalf("third agent at the same address: %v, want a %q rejection", err, ScopeStreamByIP)
	}
	if _, err := limiter.AcquireStream("agt_3", "198.51.100.4"); err != nil {
		t.Fatalf("same agent from another address: %v", err)
	}
}

// TestAddressStreamRejectionDoesNotHoldTheAgentSlot: a slot taken for a request
// that was refused anyway would stay held and silently shrink the real cap.
func TestAddressStreamRejectionDoesNotHoldTheAgentSlot(t *testing.T) {
	limiter := New(Config{StreamsPerClient: 1})

	held, err := limiter.AcquireStream("agt_1", "203.0.113.7")
	if err != nil {
		t.Fatalf("first slot: %v", err)
	}
	// Refused by the address bucket after the agent bucket already admitted it.
	if _, err := limiter.AcquireStream("agt_2", "203.0.113.7"); !errors.Is(err, ErrLimited) {
		t.Fatalf("second agent: %v, want ErrLimited", err)
	}
	held()

	// agt_2 never held a slot, so its own budget must be intact.
	release, err := limiter.AcquireStream("agt_2", "203.0.113.7")
	if err != nil {
		t.Fatalf("agt_2 after the address freed up: %v", err)
	}
	release()
}

// TestRejectionScopeNamesTheBucketThatFired: the rollback criterion asks whether
// an address-keyed limit is hitting distinct legitimate users, which is
// unanswerable if both debate buckets report the same scope.
func TestRejectionScopeNamesTheBucketThatFired(t *testing.T) {
	limiter := New(Config{DebatesPerHourPerAgent: 1, DebatesPerHourPerIP: 3})

	if _, err := limiter.AllowDebateCreation("agt_1", "203.0.113.7"); err != nil {
		t.Fatalf("first debate: %v", err)
	}
	var limitErr *LimitError
	_, err := limiter.AllowDebateCreation("agt_1", "203.0.113.7")
	if !errors.As(err, &limitErr) || limitErr.Scope != ScopeDebateByAgent {
		t.Fatalf("agent's own budget: %v, want scope %q", err, ScopeDebateByAgent)
	}

	// Distinct agents exhaust the shared address budget instead.
	for i := range 2 {
		if _, err := limiter.AllowDebateCreation(fmt.Sprintf("agt_other_%d", i), "203.0.113.7"); err != nil {
			t.Fatalf("debate from a fresh agent %d: %v", i, err)
		}
	}
	_, err = limiter.AllowDebateCreation("agt_fresh", "203.0.113.7")
	if !errors.As(err, &limitErr) || limitErr.Scope != ScopeDebateByIP {
		t.Fatalf("shared address budget: %v, want scope %q", err, ScopeDebateByIP)
	}
}

// TestRejectionLoggingIsSampled: rejections happen exactly when a client is
// flooding, so an unsampled line per request lets an attacker drown the signal
// the rollback criterion reads.
func TestRejectionLoggingIsSampled(t *testing.T) {
	var logs bytes.Buffer
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	limiter := New(
		Config{RegistrationsPerHourPerIP: 1},
		WithClock(func() time.Time { return now }),
		WithLogger(slog.New(slog.NewTextHandler(&logs, nil))),
	)

	const flood = 500
	for range flood {
		_, _ = limiter.AllowRegistration("203.0.113.7")
	}
	lines := strings.Count(logs.String(), "лимит: запрос отклонён")
	if lines == 0 {
		t.Fatal("a flood of rejections produced no log line at all")
	}
	if lines > logSampleBurst {
		t.Fatalf("logged %d lines for %d rejections; sampling is not bounding the volume", lines, flood)
	}

	// The suppressed ones are counted, not lost.
	logs.Reset()
	now = now.Add(logSampleInterval)
	_, _ = limiter.AllowRegistration("203.0.113.7")
	if !strings.Contains(logs.String(), "suppressed_since_last") {
		t.Fatalf("suppressed rejections were dropped silently: %s", logs.String())
	}
}

// TestRefundAfterTheTokenPeriodDoesNotInflateTheBucket pins the time-dependent
// half of the refund: a token returned after its period has elapsed is a no-op
// rather than an extra allowance.
func TestRefundAfterTheTokenPeriodDoesNotInflateTheBucket(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	limiter := New(Config{DebatesPerHourPerAgent: 2}, WithClock(func() time.Time { return now }))

	grant, err := limiter.AllowDebateCreation("agt_1", "")
	if err != nil {
		t.Fatalf("first debate: %v", err)
	}
	// The bucket refills on its own before the refund lands.
	now = now.Add(time.Hour)
	grant.Refund()

	for i := range 2 {
		if _, err := limiter.AllowDebateCreation("agt_1", ""); err != nil {
			t.Fatalf("attempt %d of the full allowance: %v", i+1, err)
		}
	}
	if _, err := limiter.AllowDebateCreation("agt_1", ""); !errors.Is(err, ErrLimited) {
		t.Fatalf("a late refund handed out a third token: %v, want ErrLimited", err)
	}
}
