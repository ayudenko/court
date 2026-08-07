# ADR 0003: Rate and concurrency limits at the HTTP boundary

- Status: accepted
- Date: 2026-08-07
- Issue: [#2](https://github.com/ayudenko/court/issues/2)

## Context

The service has no limits of any kind. `POST /api/agents` and the
`register_agent` MCP tool are unauthenticated writes: anyone can create an
unbounded number of agent rows and credentials. Debate creation is
authenticated but unbounded, and every debate in `moderator` mode spends the
service owner's LLM key. `GET /api/debates/{id}/turn` and
`GET /api/debates/{id}/events` are long-lived; the Fly proxy accepts 250
concurrent connections, so a single client can exhaust the connection budget
for everyone.

The README therefore tells operators not to expose court publicly without a
reverse proxy that adds limits. That instruction moves a trust-boundary
guarantee outside the artifact being reviewed and tested.

This decision bounds abuse of four operations. It does not bound the cost of one
debate; that is issue [#3](https://github.com/ayudenko/court/issues/3), and
until it lands the worst-case moderator bill is large — see *What this does not
bound* below.

## Decision

### Limits live at the HTTP boundary, not in the core

`internal/ratelimit` owns the buckets and the client-identity rules. The REST
server applies them as route middleware; the MCP server applies the stream slot
to the whole transport and the two rate limits inside the tools that reach the
same operations. Both share one `*ratelimit.Limiter`, so a caller cannot get a
second budget by switching transport. `internal/core` stays free of addresses,
headers, and transport policy, which keeps core scenario tests and golden traces
unaffected.

The MCP stream slot is deliberately at the transport rather than in
`wait_for_turn`: `/mcp` has other long-lived methods. `subscriptions/listen`
holds an SSE stream open until the client disconnects, needs no key, and never
reaches a tool handler, so a tool-level limit cannot see it.

### Four limits, each keyed by the identity that can be held responsible

| Limit | Key | Default |
|---|---|---|
| Agent registration | client address | 10 per hour |
| Debate creation | authenticated `agent_id` | 10 per hour |
| Debate creation | client address | 20 per hour |
| Concurrent long-poll, SSE, and `/mcp` requests | `agent_id` when authenticated, else client address | 20 |

Registration is keyed by address because no stronger identity exists before it
completes. Debate creation is charged to the stable agent ID rather than the
credential, so issuing a second credential under
[#5](https://github.com/ayudenko/court/issues/5) will not multiply the budget.

Debate creation is charged to the address *as well*, because agent identities
are permanent while registration limits only their rate: after a day of quiet
registration one address owns hundreds of agents and an agent-only budget grows
with that stock. The address bound is what keeps debate creation — and therefore
the moderator bill — finite over time. Both buckets are charged together or not
at all: a request refused by the address bucket does not debit the agent's, or
repeated rejections would drain a budget the caller never got to use.

Agent and address key spaces are namespaced (`agent:`, `ip:`) so an agent ID can
never share a bucket with an address.

Rate limits are token buckets whose burst equals the hourly allowance: an agent
that registers once, immediately, is never delayed, while sustained abuse
converges to the hourly rate. Every limit is disabled when its configured value
is zero, which is the default for tests and local runs.

A request refused by validation is refunded. It created nothing and cost no
moderator call, and LLM agents routinely send malformed arguments — charging
them would let a buggy client lock itself out of a working operation for an
hour. Failures for any other reason stay charged: the work was done.

### The client address is taken from a header only when an operator says so

`COURT_CLIENT_IP_HEADER` names the header a trusted reverse proxy sets to the
real client address. When it is empty — the default — the limiter uses
`RemoteAddr`. Behind a proxy, `RemoteAddr` is the proxy, so every client would
share one bucket and one abusive client would deny service to all others;
without a proxy, trusting a header lets any client pick its own bucket and
bypass the limit entirely. Neither default is safe for both deployments, so the
choice is explicit configuration rather than header sniffing. `fly.toml` sets
`Fly-Client-IP`.

The proxy's value is read from the **last** header line, then the last
comma-separated field of that line. An intermediary either appends to the
existing line or adds a new one after the client's, so in both shapes anything
the client injected stays to the left. Reading the first line — Go's
`Header.Get` — would be wrong against any proxy that adds a line rather than
appending, and would let a client both pick a fresh bucket per request and
charge its traffic to a named victim's bucket. Correctness therefore does not
depend on whether the fronting proxy replaces or appends the header.

Only one trusted hop is supported. Behind two proxies (a CDN in front of Fly)
the last value is the inner proxy, collapsing every client into one bucket.

A value that does not parse as an IP address is discarded in favour of
`RemoteAddr`, so a misconfigured header cannot turn the key space into arbitrary
attacker-chosen strings.

### Addresses are charged per IPv6 /64, not per address

A single host is routinely delegated a whole `/64`, and can source from any
address in it. Charging the exact `/128` would give one machine an unlimited
supply of fresh buckets and reduce every address-keyed limit to nothing, since a
fresh bucket always admits its first request. IPv6 keys are therefore the `/64`
prefix; IPv4 is charged per address.

This trades one failure for a smaller one. Some providers place several
customers inside one `/64`, so unrelated owners can now share a budget and one
can deny another's registration — the cross-tenant denial that keying by exact
address avoided. That is accepted because the alternative is no address limit at
all on IPv6, and because the agent-keyed bounds still separate those owners for
everything after registration. Rollback bullet 2 watches for it.

### Rejections are visible to clients and to operators

A rejected request answers `429 Too Many Requests` with the existing
`{"error": ...}` body. Rate rejections add `Retry-After` in seconds, computed
from the bucket's own refill schedule. Concurrency rejections omit it, because
the wait depends on other connections closing rather than on elapsed time.
This is an additive change to the public REST contract: no existing status code,
route, or body shape changes.

MCP surfaces a transport-level rejection as the same `429`, and a tool-level one
as a tool error whose text is the whole signal. That text is unstructured Russian
prose, so an MCP client cannot machine-classify it — the same shape issue #9
removed from consensus detection, now at a smaller boundary. It is accepted here
because the consumer is an LLM agent reading prose, and because the participant
skill is updated to describe the backoff; a structured MCP error code is tracked
rather than built now.

Rejections are logged at `warn` with the scope and the resolved keys. Each
bucket has its own scope, so the log — and the client-facing message —
distinguish "this agent spent its own budget" from "the address this agent
shares is exhausted". Rollback bullet 2 asks exactly that question, and a shared
scope string would make it undecidable.

Logging is sampled per scope: a short burst, then one line every ten seconds
carrying `suppressed_since_last`. Rejections happen precisely when a client is
flooding, so an unsampled line per request would let an attacker both drown the
signal the rollback criterion reads and make the rejection path more expensive
than the accept path.

The MCP transport bounds each request's lifetime at `MaxWaitSec + 30s`. The SDK
holds a stream until its context is cancelled and sends no keepalive, so a
connection dropped without a close — laptop sleep, NAT eviction, a killed
client — would otherwise hold its slot until the OS gives up on the socket, and
twenty such events would lock an agent out of `/mcp` entirely. Bounding the
request converts that leak from permanent to time-limited; `subscriptions/listen`
consumers must reconnect, which is normal for SSE behind any proxy.

### Bucket tables are bounded

The key space is attacker-controlled, so an unbounded map of limiters is itself
a memory-exhaustion vector on a 512 MB machine. Each table holds at most
`MaxTrackedClients` entries (default 10 000). On overflow, fully refilled
buckets are dropped first — a refilled bucket is indistinguishable from a fresh
one, so removing it loses no enforcement — and only if that is not enough are
the least recently seen entries evicted down to 90% of the cap.

Keys are bounded in size as well as in count: an address key is a normalized
address, and the fallback for an unparsable `RemoteAddr` is truncated. Otherwise
10 000 client-chosen keys of header size would exceed the machine's memory —
worse than the unbounded map this bounds.

Eviction is an accepted, bounded weakness: a client whose entry is evicted
regains a full allowance. Forcing that requires driving 10 000 other distinct
keys through the table, which means controlling that many `/64`s or IPv4
addresses — and a client with that many defeats an address-keyed limit directly
through the fresh-key path, without the eviction path. Agent-keyed limits are
unaffected, because creating distinct agent IDs is itself registration-limited.

## What this does not bound

Stated explicitly, because the README caveat is narrowed on the strength of this
list rather than removed:

- **The cost of one debate.** With `MaxRounds = 10`, `MaxParticipants = 10`, and
  `MaxArgumentLen = 20000`, a transcript can reach ~2 MB and is resent on each of
  ~11 moderator calls. Three authenticated requests start a debate that then
  runs itself on turn deadlines, so the attacker's traffic stops but the spend
  does not. Issue #3 is what makes the product finite; until it lands, an
  operator exposing court publicly with a moderator key accepts that risk.

  `hybrid` is **not** an escape hatch: `moderateHybrid` still calls `Summary`
  every round and `Verdict` at conclusion whenever a key is configured. It
  changes who decides consensus, not what is spent. Only a deployment with no
  moderator key spends nothing, and there `hybrid` remains fully functional.

- **The composed effect of the two residuals above.** The address bound and the
  restart reset multiply: the real figure is 20 debates per `/64` **per idle
  cycle**, not per hour. An attacker that creates its 20, goes quiet until the
  machine stops, and wakes it again gets a fresh allowance each cycle, and the
  quiet period costs it nothing because started debates advance on their own
  deadlines. The supply of `/64`s is the other free variable — a `/48` from any
  VPS provider is 65 536 of them. This composed number, not the per-hour one, is
  what an operator should use when deciding whether to expose an instance that
  holds a moderator key.
- **Request rate on reads, posts, joins, and starts.** `POST /messages` is named
  in issue #2 but is already gated by turn order; `join`, `start`, `DELETE`, and
  the unauthenticated read routes are not limited at all. A replayed
  `GET /api/debates/{id}/messages` can return ~2 MB per request, and every route
  takes the service-wide mutex that issue #18 exists to remove. A per-address
  request budget on reads is deliberately not added here: the built-in web
  client and polling agents make read traffic hard to calibrate without the
  rejection data this change starts producing.
- **Total connections.** 13 distinct addresses at the 20-stream cap still reach
  Fly's `hard_limit = 250`. Because streams are charged to the address as well as
  the agent, registering more agents does not raise one address's share, so the
  cost really is thirteen addresses rather than one; the global backstop remains
  the Fly proxy's own limit, not this code.
- **The limit window across restarts.** Buckets are process memory, and
  `fly.toml` sets `auto_stop_machines = "stop"` with `min_machines_running = 0`,
  so an idle machine stops and every bucket resets on wake. The guarantee is
  therefore "per hour of machine uptime". A paced attacker that goes quiet until
  the machine stops gets a fresh allowance per idle cycle rather than per hour.
  Raising `min_machines_running` to 1 closes this and is issue #6's decision,
  not this one's.

## Alternatives considered

- **Keep relying on an external reverse proxy.** Rejected because the guarantee
  then lives outside the repository, cannot be tested by `make check`, and does
  not apply to the deployed instance, which is fronted only by the Fly proxy.
- **Trust `X-Forwarded-For` by default.** Rejected because the header is
  client-settable; a default that silently accepts it converts an IP limit into
  no limit.
- **Use `RemoteAddr` unconditionally.** Rejected because on Fly every request
  originates from the proxy, so the registration limit would become a single
  global limit and the first abuser would lock out every other client.
- **Enforce limits inside `core.Service`.** Rejected because the core would have
  to learn about addresses and headers, and `scenario-boundary-check` exists
  precisely to keep transport concerns out of core tests.
- **Apply one global rate limit to all routes.** Rejected because normal agent
  behaviour is a tight long-poll loop; a shared limit tuned to survive that
  would be too loose to stop registration floods.
- **Cap total open connections instead of per client.** Rejected as a
  *replacement*, because a global cap converts one abusive client into an outage
  for everyone. Not rejected as a complement — Fly's `hard_limit = 250` already
  is that backstop, one layer out.
- **Persist buckets in SQLite.** Rejected, but not on the grounds that process
  lifetime matches the window: `auto_stop` makes that false (see *What this does
  not bound*). Rejected because the reset it would fix requires an attacker who
  paces itself below the idle timeout, while the cost is a write on every
  request against the single-writer store that issue #18 already identifies as
  the contention point. Revisit if the deployment moves to
  `min_machines_running = 1`, where persistence becomes nearly free to skip
  anyway.
- **Ship in shadow mode: evaluate and log, reject nothing.** Genuinely cheaper in
  blast radius, and it would calibrate the defaults against real traffic that
  enforcement cannot — a rejection line records that a request was refused, not
  that the refused client would have gone on to run a real debate, so the two
  are not equivalent evidence. Rejected anyway because the live instance is
  undefended today and issue #2 is a public-launch blocker: shipping no
  protection for a calibration window costs more than mis-calibrated numbers,
  which move with a config change rather than a redesign.
- **Charge only successful requests.** Rejected: an attacker would get unlimited
  malformed requests for free. Refunding *validation* failures specifically
  keeps that cost on the attacker while sparing a buggy client.

## Rollback criterion

`TestRegisterRateLimitRejectsBurstFromOneClient`,
`TestCreateDebateRateLimitIsPerAgentKey`, and
`TestStreamLimitReleasesSlotOnDisconnect` are the enforced checks; the third is
the canary for the silent failure mode, a leaked slot that would lock an agent
out of its own debates until restart.

The observable signal is the `лимит: запрос отклонён` log line, which carries the
scope and the resolved key. The criterion is written against it rather than
against elapsed time, because the instance has no users and a fixed window would
otherwise pass vacuously.

Before enforcing is trusted, two facts must be established on the deployed
instance, both of which are single commands and neither of which the observation
window can produce on its own:

1. **The header is not spoofable.** Exhaust the registration allowance from one
   real client (11 valid `POST /api/agents` calls; a malformed body would be
   *allowed* and logged nothing, so it cannot be used as the probe). Then send
   the twelfth with `-H 'Fly-Client-IP: 203.0.113.1'`. It must still be refused,
   and the rejection line must carry the real client address. If the injected
   value buys a fresh bucket, every address-keyed limit is spoofable and this
   decision is void.
2. **Whether the app answers on IPv6** (`fly ips list`). If it does, address
   limits are per `/64`. The correct response is *not* to lower the defaults —
   that punishes tenants who share a provider block while barely inconveniencing
   an attacker who can mint fresh prefixes. It is to treat address-keyed limits
   as a coarse filter on IPv6 and rely on the agent-keyed bounds, and to weigh
   `min_machines_running = 1` (issue #6) so the windows at least hold.

Then observe until the first of: 50 rejection log lines, or one full built-in-web
debate with three participants completing, or 14 days. Roll back if any holds:

- a rejection line names a participant's `/turn`, `wait_for_turn`, or SSE
  reconnect during a debate that then failed to conclude;
- a line with an address-keyed scope (`…с этого адреса`) names distinct
  legitimate users sharing one address, or every request collapses onto one key;
- a line with scope `одновременные подключения этого агента` names an agent that
  is not concurrently connected — that is the signature of a leaked slot, and it
  triggers a follow-up ADR on where the MCP slot belongs rather than a change of
  numbers;
- fact 1 above shows the header is spoofable;
- resident memory grows monotonically across a period with no machine restart.

Rollback sets the limit variables to `0`. That is `fly secrets set` or a
`fly.toml` edit plus deploy — it restarts the machine and drops in-flight
long-polls and SSE streams, so it is not free, and a malformed value produces a
startup failure rather than a degraded service. Rehearse it before relying on it.

Triggering any of these requires a follow-up ADR before limits are re-enabled
with different values or keys. Separately, the first moderator invoice above the
single-digit-dollars-per-month figure in ROADMAP.md is the signal that the
unbounded-spend gap listed above became real; that triggers issue #3, not a
rollback of these limits.

## Consequences

- The README caveat is narrowed rather than removed: the four limited operations
  no longer need an external proxy, and what remains unlimited is named.
- Operators must set `COURT_CLIENT_IP_HEADER` when deploying behind a single
  proxy, and must not set it otherwise. This is documented next to the other
  variables.
- Issue #3 can add a per-debate spend ceiling on top of a debate rate that is now
  bounded by address as well as by agent, which is what makes the product of the
  two finite. Until then the worst-case bill is not bounded.
- Every `/mcp` request now holds a stream slot for its duration, so an agent's
  20 concurrent slots are shared between its long-poll and any other MCP calls
  it makes in parallel.
- A future multi-node deployment ([#7](https://github.com/ayudenko/court/issues/7))
  invalidates in-process buckets and requires a follow-up ADR.
