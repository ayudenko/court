# ADR 0004: Per-debate spend ceiling for the LLM moderator

- Status: accepted
- Date: 2026-08-07
- Issue: [#3](https://github.com/ayudenko/court/issues/3)
- Builds on: [ADR 0003](0003-http-rate-limiting.md)

## Context

The server-side moderator calls an LLM once per round boundary plus once for the
verdict, and each call resends the whole transcript. The key those calls are
billed to belongs to whoever runs the instance, not to whoever started the
debate.

ADR 0003 added rate and concurrency limits at the HTTP boundary and listed, in
"What this does not bound", the gap this ADR closes: with `MaxRounds = 10`,
`MaxParticipants = 10` and `MaxArgumentLen = 20000`, a transcript reaches ~2 MB
and is resent on each of ~11 calls. Request-rate limits cannot bound that,
because the cost is not driven by requests. A debate advances on turn deadlines:
`expireTurns` runs on a 2-second ticker, writes a skipped-turn message and calls
`advanceTurn`, which hands the round boundary to `moderate` in its own goroutine
with `context.WithoutCancel`. After the few requests that create and start a
debate, the client can disconnect and the spend continues on its own.

`hybrid` is not an escape hatch: `moderateHybrid` still calls `Summary` every
round and `Verdict` at conclusion whenever a key is configured. Only a
deployment with no moderator key spends nothing.

No malice is required. The same spend comes from an honest 10-round debate, from
an agent that stops answering (turns expire, the moderator still runs), or from
an open browser tab, because the built-in web client mints an organizer identity
on first use. The missing property is not "protection from an attacker" but
"one debate has a finite, known cost". An attacker is only the worst case of the
same gap.

Registration is open (`POST /api/agents` needs a name and takes no
authentication), so the population that can trigger this is everyone.

## Decision

### A budget in tokens, per debate, checked before the call

`core.ModeratorBudget{DebateTokens, OutputPerCall}` caps the cumulative
moderator spend of one debate. `courtd` reads `DebateTokens` from
`COURT_MODERATOR_DEBATE_TOKEN_BUDGET` (default 500 000) and sets
`OutputPerCall` to the same `max_tokens` the provider is constructed with, from
one shared constant.

The two sides of the ceiling use different units on purpose — **bytes for
admission, tokens for accounting** — because they answer different questions.
Accounting has to reconcile against the real bill, which is denominated in tokens.
Admission has to happen before any token count exists, and the only exact quantity
available then is the byte length of what will be sent:

- **Admission, before the call:** an upper bound, not an average —
  `len(question) + len(transcript) + 4096 + OutputPerCall`, i.e. **one token per
  byte** plus a fixed reserve for the moderator's own prompt. Byte-level BPE
  (Claude, cl100k/o200k) starts from a per-byte split and only merges pairs, so
  the token count of an input never exceeds its byte count on any input at all.
  Dividing by an average ratio was rejected: the transcript is written by an
  untrusted party, and content that tokenizes worse than average is free to
  produce. Codepoints that fall back to byte tokens reach ~1 token per byte,
  which is what makes a divisor of 3 unsound and this bound sound.
- **Accounting, after the call:** the provider's reported `usage`.
  `llm.Provider.CallTool` returns `Usage` alongside its result, and
  `core.Moderator` propagates it. Negative components are clamped to zero, so a
  response reporting `{input: 1, output: -100}` cannot cancel its own charge.
  When a response carries no usage at all, the service charges its own upper
  bound instead: treating unknown spend as zero would let a provider that omits
  `usage` remove the ceiling entirely.

Because admission bounds the next call from above and accounting charges what was
actually spent, the guarantee is exact rather than approximate: **reconciled
spend never exceeds `DebateTokens`**, for both reporting and non-reporting
providers. What ordinary text does not do is reach that bound — Cyrillic runs
about five bytes per token, so a real debate spends far less than the budget it
reserves. The budget must therefore be chosen with that slack in mind, which is
what the default accounts for.

Usage is returned **together with the error** on every path where a response was
received: a call the model answered with an unparseable result is paid for, and a
ceiling that only charged successful calls would let a misbehaving provider spend
without limit.

`Usage.Billed` answers a narrower question than "did we get an answer" — it
answers "did this call enter the bill":

- a response arrived — billed, with the numbers;
- **we** stopped waiting (the three-minute moderation timeout, or a cancelled
  context when the machine stops) — treated as billed with no numbers, so the
  upper bound is charged. The request was accepted and the answer was most likely
  generated in full; we simply did not collect it. Assuming otherwise would let a
  provider slower than the timeout serve unlimited uncounted calls, which is the
  ~11-calls-per-debate figure this ADR exists to bound;
- the request never arrived (connection failure, 429 or 5xx before work) — not
  billed, not charged. Charging these would let provider downtime exhaust the
  budget of a debate that spent nothing and write a false "budget exhausted"
  record into its transcript.

Providers distinguish the last two by `ctx.Err()`: a non-nil context error means
the cancellation was ours.

The check is admission, not detection: `moderationAllowed` refuses the call
before it happens. A ceiling verified after the fact bounds nothing, because the
call it would have refused is already paid for.

### The default and the envelope it covers

500 000 admits, without degrading:

- a 3-round, 2-participant debate by a wide margin;
- a 10-round, 5-participant debate with ~2 000-character arguments — the largest
  shape a reasonable operator is likely to call ordinary. Its reconciled actual
  spend is ~226 000 tokens, while its worst admission check reserves ~394 000.

A debate at the validation limits (10 participants × 20 000-byte arguments,
200 000 bytes per round) is admitted for two rounds and degrades at the third,
having spent ~121 000 tokens. That is the intended behaviour: the pathological
shape is cut off early at a known cost, and the ordinary shape completes.

### Spend survives restart

The counter lives in `debates.moderator_tokens`, incremented by SQLite
(`SET moderator_tokens = moderator_tokens + ?`) rather than written from a
`Debate` copy that `UpdateDebate` may hold from before the previous charge.

This is deliberate divergence from ADR 0003, whose in-memory buckets reset when
the machine stops and whose guarantee is therefore "per hour of machine uptime".
`fly.toml` still has `auto_stop_machines = "stop"` and
`min_machines_running = 0`; a spend ceiling with the same property would be
defeated by waiting, because a paced debate outlives an idle cycle and resumes
through `recover`. `TestModeratorSpendCeilingSurvivesRestart` is the guard.

### Exhaustion degrades to a deterministic verdict

When the budget cannot cover the next call:

- intermediate summaries are skipped and the debate keeps running. Participant
  turns cost the server nothing, so the debate is not truncated;
- the verdict is deterministic, authored by `система` rather than the moderator;
- the protocol records a `system` message naming budget exhaustion. That is a
  protocol-visible outcome, which is why it is written into the transcript rather
  than only logged: a reader must be able to tell a model's verdict from a
  fallback.

**What the deterministic verdict is differs by mode, and this is a deliberate
departure from the wording of issue #3** ("degrade to a vote-counted verdict, as
in `hybrid` with no key"):

- in `hybrid`, votes *are* the mode's consensus mechanism, so `hybridVerdict`
  counts them as it already does when the moderator is unavailable — with one
  change: the tally leader's message is **not** quoted into the final answer when
  the trigger is budget exhaustion. Quoting it is right when a provider failed,
  because nobody chose that; it is wrong when a participant chose it, and
  exhausting the budget by writing long arguments is something any participant can
  do;
- in `moderator` mode, votes are **not** used. Consensus keeps whatever value the
  paid round summaries established, and the verdict states that no model verdict
  was produced.

Counting votes in `moderator` mode was implemented first and rejected on review,
for two reasons. It corrupted the record: a round summary that had already been
paid for and had found consensus would be contradicted by a verdict recomputed
from votes, because a participant who declares no support counts as supporting
itself, so a two-party debate always tallies as disagreement. And it moved the
outcome to whoever shows up: joining a debate requires no invitation, so a
coalition could exhaust another operator's budget by posting long arguments and
then decide both the `consensus` flag and — through `hybridVerdict`, which quotes
the tally leader's last message — the text recorded as the final answer. Neither
is acceptable in a mode whose contract is that the moderator decides.

### The counter is operator telemetry, not protocol

`Debate.ModeratorTokens` is `json:"-"` and is absent from the versioned export
schema, so schema v1 and the golden traces are untouched. Spend is reported
through structured log lines per call and per concluded debate.

### Existing round and reply limits are left alone

Issue #3 also asks for hard `rounds` and reply-length limits. Those already
exist (`MaxRounds = 10`, `MaxArgumentLen = 20000`, validated in `CreateDebate`
and `PostArgument`) and are not re-tightened here: with a spend ceiling in
place, lowering them would restrict legitimate debates without changing the
bound that matters.

## What this does not bound

- **Total spend across debates.** The ceiling is per debate. What limits the
  number of debates is ADR 0003's creation limit, and that one resets when the
  machine stops. The composed figure an operator should use is 20 debates per
  `/64` per idle cycle × the per-debate ceiling — with the default, 10 M tokens
  per `/64` per idle cycle, and a `/48` from any VPS provider holds 65 536 of
  those prefixes.
- **A prompt template that outgrows its reserve — now checked.** The estimate
  reserves `ModerationPromptOverheadBytes` (4096) for the system prompt,
  instructions and tool schema, which are not part of the question or the
  transcript. `TestFixedPromptBytesFitTheBudgetReserve` measures the fixed part of
  every request, at the longest permitted moderator name, and fails if it exceeds
  the reserve. The residual is that the reserve is compared in tokens against a
  byte measurement, which is conservative in the safe direction.
- **One call per process death.** The charge is applied after the provider
  returns. If the process dies mid-call, `recover` re-runs moderation and the
  original call was never counted. This is one unaccounted call per crash, not a
  loop, because the retry is charged normally.
- **Double charging on a clean shutdown.** The mirror image of the above: a call
  cancelled by shutdown is charged its upper bound, and the retry after restart is
  charged again. The ceiling errs toward over-charging here, which can degrade a
  debate that would otherwise have finished. Each call gets its own
  `moderationTimeout` rather than sharing one deadline across the pass, so at
  least a slow round summary can no longer hand the verdict a dead context and
  have it charged for a request that was never sent.
- **A billed response lost after the model finished.** `Billed` is decided by
  whether *our* context expired. A failure that happens after the provider did the
  work but before we could read the response — a reset connection mid-body, an
  undecodable payload — leaves our context live, so it is classified as "never
  arrived" and charged nothing while the provider bills it. Up to one such call per
  round boundary can go uncounted. Not reachable by an adversary, who controls
  neither the network to the provider nor its encoding, but it is the direction in
  which the exact bound fails.
- **Retries inside one call.** Both SDKs retry by default, so one admitted
  `CallTool` can issue up to three HTTP requests. Retryable statuses are normally
  unbilled, but a transport timeout followed by a retry can bill twice for one
  charge.
- **Participant display names inside a hybrid degraded verdict.** The vote list is
  part of the verdict text, and a participant chooses its own name. The
  leader line names the winner by `agent_id` on the exhaustion path for that
  reason, but names in the tally itself remain — votes are what that mode decides
  by, and names appear throughout the transcript anyway.
- **A provider that ignores the output cap.** `OutputPerCall` is trusted to hold
  because it is the `max_tokens` sent with the request. The Anthropic path sends
  `max_tokens`; the OpenAI-compatible path sends `max_completion_tokens`, which an
  older compatible server may ignore. Where it is ignored, one call's output can
  exceed its reserve, and the reconciled figure for that call lands above the
  budget. Bounded to one call, since admission then refuses everything after it.
- **A budget below the minimum viable value.** Anything at or under
  `ModerationPromptOverheadBytes + OutputPerCall` refuses the first call of every
  debate, which is moderation switched off rather than limited. `courtd` refuses
  to start on such a value instead of running silently, but nothing stops a
  caller of `core.NewService` from setting one.
- **A charge lost to a storage failure.** `chargeModeration` updates its
  in-memory copy and then increments the row; a failing increment is logged only.
  The ceiling stays correct for the rest of that moderation pass, but a restart
  would hand the debate the lost amount back.
- **The envelope in languages that pack more tokens per byte.** The admission
  bound is in bytes while the budget is in tokens, so the ordinary-shape envelope
  above holds for text at roughly five bytes per token. The same shape written in
  a script that tokenizes at two or three bytes per token spends proportionally
  more real tokens for the same byte volume and degrades a round or two earlier.
  The rule an operator can rely on across languages is the byte one: the default
  admits a debate whose full transcript stays under ~250 KB.
- **Instance-wide spend.** Only per-debate spend is bounded, so the headline
  guarantee is that the per-debate slice is finite, not that the operator's total
  exposure is. A per-window ceiling for the whole instance, and having the debate
  creator supply their own moderator key, are both plausible answers to the wider
  problem and neither is implemented or tracked yet.
- **Money.** Tokens are not cost. The same budget buys very different amounts
  across providers and models, and cache-read input tokens are counted at full
  weight because the provider reports them separately and court does not use
  prompt caching. Converting to currency is deliberately left out: a per-model
  price table in the repository would rot silently.
- **The `Stream` path.** Only `CallTool` is metered. `Stream` is used by
  `cmd/demo-agent`, which spends the agent's own key, not the server's.
- **External auditability of spend.** Because the counter stays out of the
  export schema, a protocol consumer cannot verify what a debate cost. Only the
  operator can, from logs or the database. Note that the export schema already
  carries an unused optional `execution` object with token fields on both summary
  and verdict records, so publishing spend later is additive and needs no version
  bump — the reason it is not done here is scope, not schema cost.
- **Machine-readable detection of degradation.** A degraded verdict is
  distinguishable only by its `speaker_name` and the Russian `system` notice;
  there is no typed flag. An operator who names the moderator `система` through
  `COURT_MODERATOR_NAME` erases even that distinction.
- **Providers that misreport `usage`.** The accounting trusts the number the
  provider returns. A provider under-reporting its own usage would shift the
  effective ceiling upward.

## Alternatives considered

- **Enforce on actual usage only, after each call.** Rejected: this detects
  overspend instead of preventing it, and the first call on a 2 MB transcript is
  exactly the one that must not happen.
- **Count transcript bytes and call count instead of tokens.** Both are exactly
  measurable in core with no estimation error, which is genuinely attractive.
  Rejected as the enforcement unit because neither is what the bill is
  denominated in, so the ceiling would have to be re-derived per provider
  anyway. Bytes survive as the admission bound.
- **Divide bytes by an average bytes-per-token ratio.** Rejected: the input is
  attacker-chosen, and a ratio that holds for prose does not hold for content
  picked to defeat it. An earlier revision of this decision used `bytes/3` and
  claimed base64 as the worst case; that was wrong twice over — base64 sits near
  the divisor, byte-fallback codepoints reach 1 token per byte, and the resulting
  ceiling would have delivered up to three times the configured budget.
- **Put the budget in the moderator or provider layer as a decorator.**
  Rejected: degradation is a state-machine outcome and the counter must be
  persisted with the debate. A decorator would have to signal exhaustion back to
  core through an error, and core would then have to distinguish it from a
  transport failure to pick the right degradation.
- **Conclude the debate immediately on exhaustion.** Rejected: further
  participant turns are free to the server, and cutting the debate short
  destroys protocol record for no saving.
- **Store the counter in the exported protocol.** Rejected: a schema v1 change
  plus golden-trace regeneration for a number that only the key's owner needs.
- **Keep relying on the operator not to expose a key.** Rejected: that is the
  status quo the README caveat describes, and it makes the public instance
  unusable as a demonstration of the product.
- **Send the moderator less instead of capping what it may spend.** The cost is
  quadratic in rounds because every call resends the whole transcript; sending
  only the current round plus the stored round summaries would make per-call input
  independent of round count and cut the ordinary shape well below ~226 000
  tokens, with no new column, no widened interfaces and no degradation outcome.
  Not rejected on merit — it is a different change. It alters what the moderator
  sees and therefore what it concludes, so it needs the golden-trace harness to
  show that consensus detection and citation accuracy survive, whereas a ceiling
  changes no moderation input at all. A ceiling is also still needed afterwards: a
  smaller per-call context lowers the cost of a debate without making it knowable
  in advance. Worth doing next, on evidence, rather than instead.

## Rollback

Reverting is a code revert. The column is additive with `DEFAULT 0`, so the
previous binary keeps reading and writing the table; the store fingerprint test
covers that. Setting `COURT_MODERATOR_DEBATE_TOKEN_BUDGET=0` disables the
ceiling without a deploy and logs a warning that spend is unbounded.

## Rollback criterion

Two observations falsify this design and require a follow-up ADR:

1. **The unit or the number is wrong.** A debate that a reasonable operator
   considers ordinary degrades before its last round under the default ceiling,
   or a degraded verdict is recorded for a debate whose reconciled actual spend
   stayed under the ceiling.
How either is noticed: the per-call `расход модератора` line carries the charged
amount and whether the provider reported it, and every concluded debate logs
`расход модератора за дебаты` with `degraded`. Criterion 1 is therefore countable
from logs; criterion 2 is only checkable against the provider's own billing when
the provider reports no usage, because then court never learns the real figure. A
debate abandoned before conclusion logs per-call lines but no summary line.

2. **The bound is not exact after all.** Any debate whose reconciled actual spend
   exceeds `DebateTokens`. The admission bound is claimed as an upper bound on
   real tokens, so a single counterexample — a tokenizer that emits more tokens
   than input bytes, or prompt overhead beyond its reserve — falsifies the design
   rather than merely the number.

## Consequences

- One debate has a finite cost that an operator can state in advance. The
  operator's *total* exposure is still open-ended — it is the per-debate ceiling
  times whatever the debate-creation limit allows, and that limit resets on
  restart. What changes is that the unbounded part is now a product of two stated
  numbers instead of a single unknown.
- `moderator` mode gains a second way to conclude without a model verdict, and
  the transcript now has to be read with that in mind.
- `llm.Provider` and `core.Moderator` are wider: both carry usage, and every
  implementation must report it.
- Spend becomes observable per call and per debate in logs, which is what makes
  the rollback criterion checkable at all.
