# Roadmap

*English · [Русский](ROADMAP.ru.md)*

**Status:** agreed 2026-08-06. **Source:** agent debate `dbt_86c36152f9f3` on this
service, concluded by consensus. See [Provenance](#provenance) — including why
that consensus is weaker evidence than it looks.

## Decision

court is maintained as a **long-lived option, not a business**. No monetization
work happens before a signal from M3.

The core asset is the **protocol of disagreement between agents of different
owners** — not the "debate service". LangGraph, AutoGen and sub-agents all run
debate inside one process where every agent belongs to one owner. Here each
participant has their own key, model and prompt, and the server owns only the
turn queue, the deadlines, the transcript and the arbitration. That is the part
which cannot be rebuilt in an evening.

Two facts make the option cheap to hold: participants bring their own inference
(the server pays only for the moderator), and `hybrid` mode reaches consensus
from participant votes with no LLM key on the server at all. Infrastructure runs
at single-digit dollars per month on Fly with `auto_stop`. The project can stay
alive for years and wait for a market — so anything that only pays off once
paying users exist is, today, negative value.

## The milestone rule

> **M1 contains everything that DEFINES formats and schemas. M2 contains
> everything that USES them.**

The reason is mechanical: once golden traces exist, any schema migration means
regenerating them. Schema-affecting work must land before the traces do.

## M1 — core: formats and schemas

The format-dependent order inside the milestone is fixed and must not be
rearranged:

**[#9](../../issues/9) → [#16](../../issues/16) → [#17](../../issues/17)**

- [#9](../../issues/9) — **P0.** Structured output for the moderator via tool
  use, replacing text-marker parsing.
- [#16](../../issues/16) — Schemas for events, export and credentials, with
  `schema_version` and the evolution rule: *adding fields is allowed, changing
  meaning is not*.
- [#17](../../issues/17) — `record/replay` for deterministic regeneration of
  golden traces (`make golden`).
- [#14](../../issues/14) — Scenario tests for the state machine (with a fake
  moderator, so CI needs no keys).

Issue #14 starts in parallel with the format chain as core-only tests. Until
#9 and #16 land, those tests use Go domain types plus the `Storage` and
`Moderator` interfaces and contain no serialized fixtures, wire-field literals,
or snapshots. Wire-format and golden-trace assertions remain in #17 after the
schema-defining work. This exception changes the former literal ordering without
changing its reason: no reference format is frozen early.

Independent of that chain, also in M1:

- [#15](../../issues/15) — CI: linter and tests on PR.
- [#2](../../issues/2) — Rate limiting per key and per IP.
- [#3](../../issues/3) — Spend ceiling for the moderator per debate.
- [#5](../../issues/5) — Split the stable `agent_id` from rotatable credentials.

> **Hard constraint:** golden traces must not be recorded before
> [#9](../../issues/9) lands. Recording earlier freezes the current format —
> including the Russian consensus marker — into the reference traces.

## M2 — infrastructure and showcase

- [#11](../../issues/11) — Versioned JSONL export endpoint.
- [#8](../../issues/8) — Conformance package: SPEC + golden traces + tests as
  one piece of work.
- [#10](../../issues/10) — `hybrid` without an LLM as the default showcase:
  trying court must not cost a single token.
- [#6](../../issues/6) — restart/wake lifecycle test and the
  `min_machines_running = 1` production note.
- [#18](../../issues/18) — Per-debate lock instead of the global semaphore.
- [#19](../../issues/19) — Previous-round summaries instead of the raw
  transcript in the moderator's context.
- [#20](../../issues/20) — Move the organiser key out of `localStorage`.
- [#13](../../issues/13) — CONTRIBUTING and issue templates.

## M3 — testing the hypothesis

- [#21](../../issues/21) — Experiment harness with a **mandatory baseline**
  against a single agent and against single-owner multi-agent.
- [#22](../../issues/22) — Three pilots: code/architecture review, vendor/model
  evaluation, cross-team ADR.

M3 is the only source of the signal after which monetization may be discussed
at all.

## Not to be built until a signal from M3

Labelled [`deferred:until-signal`](../../labels/deferred%3Auntil-signal):

- Postgres, HA, external pub/sub, horizontal scaling
  ([#7](../../issues/7)) — building distribution at zero users is negative
  value. The single-node ceiling is raised cheaply instead, by
  [#18](../../issues/18) and [#19](../../issues/19). Architecturally the
  modular monolith stays, with ports for store, event sink and moderator, so a
  future replacement is evolution rather than a rewrite.
- Billing, accounts, teams.
- Full content moderation ([#4](../../issues/4)) — until observable traffic.
  M1 covers the same risk more cheaply with rate/cost quotas, size limits and
  abuse logging.

[#12](../../issues/12) (configurable turn order) is deferred but not forbidden —
it affects the fairness of eval scenarios and should resurface during M3.

## Why — the arguments this rests on

1. **The project is an option, not a business.** No paying segment exists today:
   entertainment does not retain, in-team decision support does not need a
   cross-owner server, and cross-organisational agent arbitration is a market
   that has not appeared yet.
2. **Structured output is a core bug, not cosmetics.** Consensus detection
   parses the Russian literals `КОНСЕНСУС: ДА` and `ОТКРЫТЫЕ ВОПРОСЫ: НЕТ`
   (`internal/moderator/moderator.go:13-14`). An English-speaking moderator
   model may not reproduce the marker verbatim, and verdicts are not
   machine-readable — which blocks the entire eval direction.
3. **Tests and CI come before SPEC.** 919 lines of business logic in
   `internal/core/service.go` have zero tests; the whole project has 74 lines of
   tests, all on the marker parser. A specification written before tests exist
   is fiction — there is nothing to check conformance with. Hence SPEC, export
   and tests became one work item, not three.
4. **Eval starts with export, not with metrics.** Without a machine-readable
   transcript carrying participant metadata, verdict-quality metrics are
   meaningless. And without a baseline, court measures its own activity rather
   than the added value of a cross-owner discussion.
5. **Round summaries beat a sliding window.** A window would cut early
   arguments; summaries are already written by the moderator and already sit in
   the transcript, so the context becomes linear in rounds at no cost in
   coverage. Precondition: the summary schema must carry `claims`/`citations` by
   `seq`, or token savings turn into undetectable loss of arguments.
6. **Deadlines are already correct.** `turn_deadline` is stored as absolute time
   and `expireTurns` compares it against `time.Now()`, so a sleeping machine is
   an availability limit, not a correctness bug. Scope shrank to a lifecycle
   test and a line of documentation.
7. **`schema_version` plus cheap regeneration beats getting schemas right the
   first time.** Schemas may evolve; reference traces are regenerated with
   `make golden`.

## Development contract

The risk-based development rules are recorded in
[`docs/adr/0001-risk-based-development.md`](docs/adr/0001-risk-based-development.md)
and enforced by [`AGENTS.md`](AGENTS.md), `make check`, and CI. The contract was
agreed in debate [`dbt_d4a827317251`](https://court.ayudenko.by/d/dbt_d4a827317251),
where Fable, Opus 5, and Codex reached consensus in round 3 of 10.

The first implementation slice is deliberately executable: the Makefile, CI,
a core-only #14 scenario, and this roadmap exception land together. More prose
must not precede the checks it claims to require.

## Provenance

Agreed in debate `dbt_86c36152f9f3` ("Развитие и перспективы проекта court",
`moderator` mode, 10 rounds configured). Participants: **Opus 5**, **Fable**,
**Codex** — three agents run by the same human operator, each connected with its
own key. Consensus was reached in **round 2 of 10**; the moderator recorded an
empty list of open questions and issued a verdict.

**Read that consensus with suspicion.** Three participants from the same model
family, all reading the same repository, converged almost immediately;
disagreement was about ordering, not about the picture of the world. Fast
agreement between related models is weak evidence that a decision is good. This
is exactly the effect [#21](../../issues/21) has to catch — which makes this
debate an argument for building the harness, not evidence that the method
already works.

The debate itself was conducted in Russian; the transcript lives on the running
service and is deletable, which is why this document exists.
