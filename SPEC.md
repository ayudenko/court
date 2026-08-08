# The court debate protocol, version 1

- Status: normative for schema version 1
- Decision record: [ADR 0007](docs/adr/0007-protocol-conformance-suite.md)
- Schema definition: [ADR 0002](docs/adr/0002-protocol-schema-v1.md)

court arbitrates a debate between agents that belong to **different owners**.
The server owns the turn queue, the deadlines, the transcript and the
arbitration; each participant brings their own model, prompt and key. This
document defines what that server must do to be called an implementation of the
protocol, and what a reader of a debate artifact is entitled to assume.

## How to read this document

This specification is written **over recorded traces**, not ahead of them. Every
statement below falls into exactly one of two classes, and the difference is
mechanical rather than editorial:

- **Normative rules** carry an identifier `C0`–`C14` and a section of their own.
  Each is enforced by a check in `internal/conformance` and by a test that
  proves the check rejects a violation. An implementation that breaks one is not
  an implementation of version 1.
- **Descriptive text** is everything else. It explains the mechanism, records
  the current behaviour and helps a reader interpret an artifact — but it binds
  nobody. Where this document describes behaviour without a rule identifier, the
  running implementation is the authority, not this text. The
  [Versioning](#versioning) section is the single stated exception.

`TestEverySpecRuleIsEnforcedAndDocumented` fails if this document states a rule
the checker does not enforce, if the checker enforces a rule this document does
not state, if a heading's wording differs from the check it names, or if a rule
has no test proving it rejects a violation.
`TestSpecStatesTheLimitsItIsCheckedAgainst` pins the numbers published here to
the constants enforced in code. A specification written before its tests is a
fiction: there is nothing to conform to when conformance cannot be checked. Those
tests are what keep this file from becoming one.

They have a limit worth knowing. They reconcile rule *identifiers*, headings and
bounds — so no rule can go missing, appear unannounced, or be left unenforced —
but they do not read rule bodies. Prose inside a rule that drifts from the check
below it is caught by review, not by `make check`.

## Reference artifacts

Seven traces in `internal/golden/testdata` are the evidence this document is
written over. They are recorded by running the real service — the same consistent
snapshot and the same producer that `GET /api/debates/{id}/export` uses — so they
cannot describe a format the server does not emit. Regenerate them with `make
golden`; never edit them by hand.

| Trace | What it is evidence of |
|---|---|
| `open_embargo_v1.jsonl` | A debate before it starts: no transcript, no votes, and no `description` |
| `preparing_phase_v1.jsonl` | The preparation phase: the context is now public, `current_round` is still 0, and nothing has been said |
| `hybrid_running_partial_v1.jsonl` | An unfinished debate mid-round: one argument, one derived vote, no verdict |
| `moderator_consensus_v1.jsonl` | Moderator mode ending early on consensus, with a structured summary and verdict |
| `hybrid_split_vote_v1.jsonl` | Hybrid mode where the participants keep their own positions and no consensus is reached |
| `hybrid_multi_round_v1.jsonl` | A debate crossing two round boundaries: summaries closing rounds 1 and 2, then the verdict in round 3 |
| `hybrid_no_moderator_v1.jsonl` | A hybrid debate on a server with no LLM key: no summary in any round, a `system` message explaining each absence, and a verdict derived from the votes |

## The artifact

A debate is published as a **canonical JSONL stream**: one JSON object per line,
each carrying `schema_version`, `record_type`, `debate_id` and exactly one typed
payload. The record types are `debate`, `participant`, `message`, `round_summary`,
`verdict` and `vote`. Their fields are defined by ADR 0002 and by
`internal/protocol`.

The canonical order is: one `debate` record, then participants sorted by
`agent_id`, then the whole transcript sorted by `seq`, then the current votes
sorted by `agent_id`. Note that participants are ordered by identifier and **not**
by turn order — see C4 for how turn order is recovered.

Any status is exportable. An unfinished debate is a valid artifact describing a
real moment, which is what an observer of a stuck debate has to work with.

### `seq` is the only ordering authority

`seq` is an opaque, strictly increasing integer within one debate. It is **not**
guaranteed to start at 1, to be contiguous, or to be comparable across debates;
the reference implementation draws it from a store-wide counter, so two debates
running at once interleave their values.

`created_at` is a timestamp, not an order. It is UTC RFC 3339 and it is
informative only: readers ordering a transcript by time rather than by `seq` are
not covered by any rule here.

### Text is a rendering, structure is the record

`round_summary.text` and `verdict.text` are human-readable renderings. Business
meaning lives in `result` — `summary` / `final_answer`, `claims` with their
`citations`, `decisions`, `unresolved_questions` and `consensus`. A consumer must
never parse `text` to recover a decision. `hybrid_split_vote_v1.jsonl` shows why:
its verdict `text` is the moderator's prose while `result.consensus` was
overwritten by the participants' votes (C9), so the two are not interchangeable.

A `round_summary` or `verdict` recorded before schema version 1 keeps its text
with `result` absent. Absent evidence is represented as absent, never
reconstructed from prose.

## The state model

A debate moves through five statuses:

```
open ──▶ preparing ──▶ running ──▶ moderating ──▶ concluded
  └──────────────────────▲            │    ▲          
                         └────────────┘    │      (next round)
                                           └── (last round or consensus)
```

- **`open`** — the roster is forming. Agents may join; nobody may speak. The
  question is public, the discussion context is not (C13).
- **`preparing`** — optional, entered at start when `prep_time_sec > 0`.
  Participants study the materials; there are no turns (C14). It ends on its own
  deadline, not on an action.
- **`running`** — participants speak in turn (C4).
- **`moderating`** — the round is over and the round result is being produced.
- **`concluded`** — terminal. Nothing is appended afterwards.

Only the creator may start a debate, and only with at least two participants. The
roster is frozen at that moment: joining requires `open`. The creator may be an
**observer** who organises the debate without speaking, so `creator_id` is not
necessarily among the participants.

Only the creator may delete a debate, which destroys the transcript with it.

## Turns and deadlines

- `turn_timeout_sec` is between 30 and 1800, default 180. Each turn gets its own
  deadline.
- `prep_time_sec` is between 0 and 3600.
- `rounds` is between 1 and 10, default 3.
- Participants are between 2 and 10 (C1).

Turn order is the order participants joined, ties broken by `agent_id`. Within a
round every participant gets exactly one turn, in that order (C4).

When a turn's deadline passes, the turn is **skipped**: a `system` message records
it and the turn moves on. A skipped turn produces no argument, so a round's
arguments are a subset of the roster rather than all of it. Deadline expiry is
polled, not instantaneous — an artifact may show a deadline already passed for a
turn still open.

A debate runs on deadlines alone once started. No participant action is required
to keep it moving, and the cost of a debate to the server is bounded before it
begins.

## Consensus

`debate.consensus` is the outcome. How it is decided depends on the mode, and the
mode's authority cannot be overridden by the other mechanism:

**`moderator`** — the server's LLM moderator decides. After each round it
produces a structured summary; consensus is reached when that summary reports
`consensus: true` **and** leaves `unresolved_questions` empty. Reaching it ends
the debate early — `moderator_consensus_v1.jsonl` is `concluded` at
`current_round: 1` of 3. Participant votes are **not** a consensus mechanism in
this mode: an agent that names no support is treated as supporting itself, so
counting votes here would hand the outcome of someone else's debate to whoever
joined it. Rule C8.

**`hybrid`** — the participants decide, by unanimity of everyone who has spoken
(at least two of them). A moderator, if configured, still writes summaries and
formulates the verdict, but it cannot change the outcome: `consensus` is
overwritten from the votes on both summaries and the verdict. This is the mode
that works with no LLM key on the server at all. Rules C9, C10.

### Votes

A vote is not a separate message type. Every argument may name a `support_id` —
the participant whose position its speaker currently backs. A participant's vote
is the `support_id` of their **latest** argument; naming nobody means standing on
their own position. Participants who have not spoken have no vote.

The `vote` records in an artifact are therefore **derived**, and any reader can
recompute them from the transcript and detect a producer that did not (C10).

## What the protocol does not guarantee

These are the assumptions a consumer must **not** make. Each is a real behaviour
of the reference implementation, not a hypothetical:

- **A concluded debate does not necessarily have a verdict.** In `moderator`
  mode, an unavailable moderator concludes the debate with a `system` message and
  no `verdict` record. C7 bounds where a verdict may appear; nothing requires one
  to exist.
- **A round does not necessarily have a summary.** A round still in progress, or
  a round that ends the debate, has none by design; otherwise the moderator may
  have been unavailable or the debate's moderator budget exhausted, and those two
  cases say so in the transcript. See below for how to tell them apart.
- **A verdict is not necessarily the model's.** A verdict whose `speaker_name` is
  `система` was produced deterministically — from the votes in `hybrid`, or as an
  explicit refusal to formulate an outcome in `moderator` mode. The reference
  implementation also records why, in the `system` message preceding it, but
  neither that message nor `speaker_name` is a rule: a moderator could in
  principle be named that way.
- **A round does not necessarily contain an argument from every participant.**
  See skipped turns above.
- **`current_round` is not the number of completed rounds.** It is the round in
  progress or last moderated.
- **Execution metadata is absent.** `model`, `prompt_version`, cost and latency
  are optional in the schema and not currently produced; court cannot observe an
  external participant's model at all. Tracked as issue #33.
- **Timestamps are not an ordering authority.** See `seq` above.

### A degradation explains itself in the transcript

**A result that a degradation took away is not silently absent.** Two causes are
published — the moderator was not used, and the debate's moderator budget was
exhausted — and both are recorded the same way in both modes: by a `system`
message in the affected round, preceding the result it explains, at most once per
round per cause. Six messages exist, and which one appears identifies the cause,
what was lost, and whether a `verdict` record still follows:

| Message | Cause | What it means | Verdict follows |
|---|---|---|---|
| `Модератор недоступен, дискуссия продолжается без промежуточного итога.` | unreachable | This round has no summary | — |
| `Бюджет модератора на эти дебаты исчерпан, дискуссия продолжается без промежуточного итога.` | budget | This round has no summary | — |
| `Модератор недоступен, дебаты завершены без вердикта.` | unreachable | `moderator` mode: the debate concludes with no verdict at all | no |
| `Бюджет модератора на эти дебаты исчерпан, итог зафиксирован без вердикта модели.` | budget | `moderator` mode: the verdict that follows is a deterministic refusal to formulate an outcome, and its `consensus` is whatever the paid summaries established | yes |
| `Модератор недоступен, итог подведён детерминированно по голосам участников.` | unreachable | `hybrid` mode: the verdict that follows was counted from the votes | yes |
| `Бюджет модератора на эти дебаты исчерпан, итог подведён детерминированно по голосам участников.` | budget | `hybrid` mode: the verdict that follows was counted from the votes, but unlike the row above its `final_answer` does not quote the leading participant's argument — it points at the transcript instead | yes |

**`unreachable` is the wider of the two causes: it means the moderator was not
used.** A provider that failed is the common reason, and a server with no key
configured is the supported one, but the service also declines to call a
moderator it can still reach when it can no longer account for what a call costs
— when the write recording a previous charge failed, or when one round has
already spent more calls on retries than the retry policy allows. The artifact
does not distinguish those, and deliberately: the alternative is to publish
`budget` for a debate whose budget is untouched, which is a claim about the
operator's spend that no consumer can check, because the spend counter is not in
the artifact. `unreachable` is the cause no stored record contradicts. An operator
who needs the distinction reads the service log, where it is a separate field.

This is what makes hybrid with no LLM key on the server — a supported deployment
in which *every* round summary is absent — a readable artifact rather than an
apparently truncated one. `hybrid_no_moderator_v1.jsonl` is that deployment
recorded.

**A summary absent for any other reason is absent by design, and says nothing.**
A round that ends the debate has no summary, because the verdict is its result: a
last round, or in `hybrid` a round the votes concluded unanimously. So the absence
of a summary is not by itself evidence of a degradation — the presence of one of
the two messages above is. `hybrid_split_vote_v1.jsonl` is the by-design case:
one round, no summary, no `system` message, a verdict.

Three limits are worth stating plainly:

- **This is not normative.** No `C` rule constrains how a degradation is
  recorded, so an artifact from another implementation that omits these messages
  still conforms. Making them normative would oblige every implementation to emit
  these particular Russian sentences, which is a property of this server rather
  than of the protocol. What the rules guarantee is only that a summary or a
  verdict may be absent (C6, C7).
- **The distinction is carried in prose `text`.** A `system` message has no
  `result`, and adding a structured degradation marker would be a field addition
  rather than a description of what the server emits today. A consumer that needs
  to detect degradation programmatically therefore has to match these strings —
  which is why they are reproduced verbatim here and pinned to the service's own
  constants by a test.
- **A skipped summary's notice is best-effort.** If writing it fails, the debate
  continues without it and a later round is unaffected. The notices that precede
  a verdict, and the one recording that no verdict will come, are not: if their
  write fails the debate stays `moderating` rather than concluding unexplained.
  The running server retries that round on its own until the write succeeds, so
  `moderating` is a transient state rather than one that waits for a restart.
  What is bounded is not the retrying but its cost: a retry stops calling the
  moderator once the debate's spend ceiling is reached, once one round has spent
  more calls on retries than the policy allows, or once a charge could not be
  recorded — the first of those produces the `budget` outcome, the other two the
  `unreachable` one. Retrying itself is free and does not stop.

## Versioning

**This section is normative by reference to [ADR 0002](docs/adr/0002-protocol-schema-v1.md)
and is the one exception to the descriptive rule above.** The evolution rule is a
decision of record, not a description of current behaviour: it is what a
consumer's decoder is written against, and reading it as non-binding would void
the forward-compatibility promise the whole schema rests on. It carries no `C`
identifier because it constrains *future versions of this document*, which no
check on a present-day artifact can enforce.

Within version 1, **fields may be added** and a consumer must ignore fields it
does not understand. A field may not be removed, renamed, retyped, or have its
meaning changed.

Adding a value to an enumeration is **not** an additive change. A new
`record_type`, message `kind`, event type, debate `mode` or `status` requires a
new schema version, because a version-1 consumer cannot interpret it safely.
Version-1 producers reject any explicit `schema_version` other than 1 and reject
unknown tags rather than passing them through.

A consumer configured to reject unknown JSON fields is not a supported version-1
decoder.

## Normative rules

Each rule below is enforced by `internal/conformance` and has a test that proves
a violating artifact is rejected. Run them with `make check`.

### C0. The artifact decodes as a canonical version 1 JSONL stream

Every line is a valid record whose `schema_version` is 1, whose `record_type`
matches its single payload, and whose `debate_id` is the same throughout. There
is exactly one `debate` record. Participant identifiers, transcript sequence
numbers and vote agent identifiers are each unique.

The records are **presented in canonical order**, not merely capable of being
sorted into it. Order is part of the artifact: a consumer reading the stream as
it arrives relies on participants preceding the transcript and votes following
it, and a producer that emits some other order is not conforming even though a
sorting reader could repair it.

An artifact larger than 16 MiB — beyond anything this protocol can legitimately
produce — is refused rather than parsed.

### C1. A started debate has between two and ten participants

A debate that has left `open` and `preparing` has at least two participants,
because it could not have been started otherwise. No debate has more than ten.

### C2. An argument names a participant; a system message has no speaker

A `message` of kind `argument` carries a `speaker_id` that appears among the
participant records. A `message` of kind `system` is the server speaking: it
carries no `speaker_id` and casts no vote.

### C3. A vote inside a message supports a participant

Where a message carries `support_id`, that identifier belongs to a participant of
this debate. An agent cannot back a position that is not in the debate.

### C4. Arguments follow turn order and no participant speaks twice in a round

Order the participants by `joined_at`, breaking ties by `agent_id`; that is turn
order. Within any one round, the arguments in `seq` order occupy strictly
increasing positions in it. A participant who missed a turn is simply absent from
that round — the rule requires a subsequence, not the whole roster.

Turn order is deliberately recoverable from the artifact even though the records
are published sorted by `agent_id`, so a reader can verify the queue was fair
rather than trust that it was.

This rule freezes join order as *the* turn order, which is the only order the
current implementation has. A debate with a configurable order (a planned
feature) cannot satisfy it, and the intended landing is additive rather than a
new schema version: the debate record declares its order and this rule reads
that declaration. Until such a field exists, an artifact that does not follow
join order is not conforming.

### C5. Rounds run from one to the declared count and never move backwards

Every transcript record is in a round between 1 and `rounds`. Rounds do not
decrease along the transcript and never run ahead of `current_round`, and
`current_round` never exceeds `rounds`. `current_round` is zero exactly while the
debate is `open` or `preparing`, and at least 1 from then on.

### C6. A round carries at most one structured summary

Two structured summaries for one round would leave a reader unable to say which
one the debate acted on. A round with none is permitted — see the degradations
above.

### C7. There is at most one verdict and it closes the transcript

A verdict is the last transcript record; nothing follows it. It appears only once
a debate has reached `moderating`, and never in `open`, `preparing` or `running`.

A verdict may be recorded while the status is still `moderating`: an
implementation that persisted the verdict and then failed to write the debate
state must fail closed with the evidence preserved, rather than lose it.

### C8. In moderator mode the debate agrees with its verdict

In `moderator` mode, a `concluded` debate that recorded a structured verdict has
`debate.consensus` equal to that verdict's `result.consensus`. One artifact must
not report two different outcomes.

### C9. In hybrid mode the votes decide and nothing may contradict them

In `hybrid` mode, a `concluded` debate has `debate.consensus` equal to unanimity
of the recorded votes — every voter supporting one position, with at least two
voters. Where a structured verdict exists, its `result.consensus` equals
`debate.consensus` too. A model formulates the outcome in this mode; it does not
determine it.

### C10. Votes are a function of the transcript

In a `hybrid` debate that has started, the vote records are exactly what the
transcript implies: for every participant who spoke, a vote for whoever their
last argument supported, defaulting to themselves; for every participant who did
not, no vote. Names in a vote match that participant's record.

This makes the vote block verifiable rather than asserted. Recompute it; a
producer that fabricated the outcome will not match.

### C11. Votes appear only in a hybrid debate that has started

There are no vote records in `moderator` mode, where votes do not decide
consensus, and none while a debate is `open` or `preparing`, before anyone could
have spoken.

### C12. Every citation points backwards at a record of this transcript

Every `seq` in a claim's `citations` is the sequence number of a record in this
transcript, and is strictly smaller than the `seq` of the record citing it.
Moderator claims stay checkable against what was actually said; header-looking
text inside a participant's message is untrusted content and never a citation
target.

### C13. The discussion context is withheld while the debate is open

A debate in status `open` carries no `description`. The context becomes public at
start — the preparation phase or the first round — so joining early buys no head
start.

The rule is exactly that, and no more. The export is **not** a mirror of the
other read paths: a participant record also carries `persona`, which no other
read path returns. That was decided deliberately by ADR 0006 and is the one field
the export adds. A future field on the participant record is a disclosure
decision of the same kind, and this rule is not evidence that the export cannot
over-disclose.

### C14. Nothing is said before the first round begins

A debate in status `open` or `preparing` has an empty transcript. Preparation is
for reading the materials, not for speaking.

## Checking an implementation

```go
import "court/internal/conformance"

violations := conformance.Check(artifactBytes) // nil means it conforms
```

`conformance.Rules()` enumerates the rules with their identifiers.

The suite judges an implementation only by the artifact it publishes: no internal
state, no private endpoint, no cooperation from the implementation beyond
producing an export. That is what makes it applicable to an implementation that
shares none of this code.
