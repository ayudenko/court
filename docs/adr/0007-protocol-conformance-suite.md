# ADR 0007: Conformance package — SPEC, traces, and tests as one artifact

- Status: accepted
- Date: 2026-08-07
- Issue: [#8](https://github.com/ayudenko/court/issues/8)

## Context

The stated asset of this project is the **protocol of disagreement between
agents of different owners**, not the debate service. Today that protocol exists
only as the behaviour of one Go program. There is no document a second
implementation could be held to, and no way to decide whether an artifact
someone else produced is a court debate.

ADR 0002 defined the versioned record schema. ADR 0006 exposed it over HTTP and
made the golden-trace generator and the export endpoint share one producer, so a
checked-in fixture cannot attest to bytes the server does not emit. What is still
missing is the layer above the schema: the schema says a `vote` record has an
`agent_id` and a `supports_id`; it does not say that the votes must follow from
the transcript, that a verdict ends the transcript, or that turn order is fair.
Those are the properties that make an artifact meaningful, and none of them is
checkable today.

Issue #8 was reframed after debate `dbt_86c36152f9f3`. The original framing —
"SPEC.md first" — was withdrawn: a specification written before its tests is a
fiction, because there is nothing to conform to when conformance cannot be
checked. The agreed framing is that SPEC, traces, and tests are **one piece of
work**, with the specification written *over* traces that already exist.

## Decision

### A rule is normative only if a check enforces it

`SPEC.md` contains two kinds of statement, separated mechanically rather than by
editorial intent:

- A **normative rule** carries an identifier `C0`–`C14`, has its own section, is
  enforced by a check in `internal/conformance`, and has a test proving that
  check rejects a violating artifact.
- Everything else is **descriptive**: it explains the mechanism and helps a
  reader interpret an artifact, and the running implementation — not the
  document — is its authority.

`TestEverySpecRuleIsEnforcedAndDocumented` fails if the document states a rule
identifier the checker does not register, if the checker registers one the
document does not state, if a heading's title differs from the registered one, or
if any rule lacks a rejection test. `TestSpecStatesTheLimitsItIsCheckedAgainst`
additionally pins the numeric bounds the document publishes to the constants the
service enforces.

### What that mechanism does and does not prove

It is worth stating the limit precisely, because the temptation is to claim more.

The tests prove that no rule can be **added, dropped, renamed, or left
unenforced** silently, and that a published bound cannot drift from the constant
behind it. They do not read rule *bodies*. A heading whose prose has drifted from
the check underneath it passes, and a check with several violation branches is
satisfied by a rejection test for any one of them.

This limit is not hypothetical. Two defects in this change were found by review
rather than by `make check`: SPEC's degradation section asserted behaviour that
`hybrid` mode does not have, and C11's check missed the `preparing` status where
C10 is also inert, so a fabricated vote block passed the whole suite. Both are
exactly the class the mechanism is meant to prevent, and it caught neither.

So the honest claim is narrower than "drift is impossible": a whole rule cannot
go missing or unenforced without failing `make check`, and the remaining drift
surface — prose inside a rule body, branches inside a check — stays a review
obligation. Every rejection case therefore requires the *exact* set of violations
rather than membership, so a case cannot survive on a neighbouring rule's firing
after its own regresses.

### Conformance judges artifacts, not implementations

`conformance.Check` takes the exported JSONL bytes and nothing else. It reads no
internal state, calls no private endpoint, and requires no cooperation beyond
producing an export. An implementation sharing none of this code can therefore
be judged by it, which is what "protocol" has to mean here.

It returns every violation rather than the first, because a conformance result
is a report about an artifact, not an assertion inside one test. Record order is
the one exception: a single misplaced record shifts every position after it, so
reporting the first mismatch says more than reporting all of them.

### The rules cover semantics the schema cannot

The schema layer already rejects malformed envelopes, unknown tags and duplicate
keys, and `C0` reuses it — extended so that record order is judged rather than
silently repaired, since the reader that normalises a permuted stream would
otherwise make a producer's ordering unobservable. The remaining fourteen
rules are the ones a valid-looking artifact can still break: participant
identity of speakers and vote targets (C2, C3), fair turn order recovered from
`joined_at` even though records are published sorted by `agent_id` (C4), round
progression (C5, C14), uniqueness and terminality of moderation results (C6, C7),
the mode-specific consensus authority (C8, C9), votes as a function of the
transcript (C10, C11), backward-pointing citations (C12), and the
context embargo before start (C13).

Two of these are load-bearing beyond bookkeeping. **C10** makes the vote block
recomputable, so a producer that fabricated an outcome does not match its own
transcript. **C9** states that in hybrid mode the participants decide and neither
the debate record nor the model's verdict may report otherwise — the property
that lets court run with no LLM key on the server.

### The reference traces cover the shapes the rules speak about

Four traces are added to the two recorded by ADR 0006: a debate that has not
started, one in its preparation phase, one exported mid-round, and one crossing
two round boundaries.
They are not decorative. The export is defined for every status, the experiment
harness (#21) needs partial artifacts to observe a stuck debate, and the
round-boundary rules (C4, C5, C6) had no artifact to hold them: every earlier
trace concluded inside round 1, so "rounds never move backwards" and "one summary
per round" were enforced only against hand-built mutations, with no artifact showing
summaries in two different rounds being accepted. All six are recorded
by running the real service through the shared producer and regenerated with
`make golden`.

Two shapes still have no recorded trace: a skipped turn after a deadline expiry,
and a budget-degraded conclusion. Both require driving wall-clock time or a spend
ceiling through a recorder that runs on a frozen clock. Their rules are exercised
by mutation tests and by the service's own tests, but not by an artifact a real
run produced. The preparation phase, which C5, C11 and C14 all speak about, is
recorded: entering it needs no clock control, only leaving it does.

### What is deliberately not normative

- **Rendered text.** `text` on a summary or verdict is a rendering; `result` is
  the record. No rule constrains `text`, and SPEC states that a consumer must
  never parse it. The hybrid trace shows the two disagreeing by design.
- **Timestamps as ordering.** `created_at` is informative; `seq` is the ordering
  authority. A rule over `created_at` would freeze an accident of the clock.
- **`seq` shape.** Strictly increasing within a debate, but not contiguous, not
  starting at 1, not comparable across debates. The reference implementation
  draws it from a store-wide counter, and pinning that would turn a storage
  detail into a public contract.
- **The existence of a verdict or a summary.** Both are absent under documented
  degradations. SPEC lists these as non-guarantees so a consumer does not encode
  the opposite assumption; C7 bounds where a verdict may appear without
  requiring one.
- **How a degradation is recorded**, including the part that *is* uniform across
  modes — a budget notice precedes the affected result in the same round, at most
  once. That could be checked: rejection is proved by mutating a conforming
  artifact, not by one that positively exhibits the rule, so the absence of a
  budget-degraded trace does not on its own prevent a rule. Two other reasons do.
  Its subject appears in none of the six recorded artifacts, which makes it a rule
  written ahead of evidence — the thing issue #8 was reframed to prevent. And the
  surrounding behaviour is one we intend to change: #36 makes hybrid degradations
  recorded rather than silent, and freezing the current shape first would turn
  that fix into a compatibility change.
- **Execution metadata.** Absent, tracked as #33.

### One rule is knowingly scheduled to be falsified

C4 freezes join order as *the* turn order. Issue #12 — configurable turn order —
is deferred but not forbidden, and the roadmap expects it to resurface during M3
because it affects the fairness of evaluation scenarios. When it ships, every
artifact from a correct run breaks C4 as written, which is rollback criterion 1
firing by design rather than by discovery.

This is accepted with the landing path stated in advance: the debate record gains
a declared turn order and C4 reads that declaration instead of assuming join
order. That is a field addition, which version 1 permits, so #12 costs a rule
amendment rather than a schema version. SPEC states the same thing so an
implementer is not surprised either. The alternative — leaving turn order
unspecified until #12 lands — was rejected because fair ordering is one of the few
properties an artifact from a stranger's server can actually be checked for, and
shipping a conformance suite that is silent about it defeats the purpose.

C9's quorum is frozen on the same terms: unanimity among whoever spoke, minimum
two. Two speakers can therefore conclude a debate whose other eight participants
were skipped. Tightening that later is a compatibility break, and it is frozen
anyway because it is what the mode's authority currently means; a quorum rule
invented here would be a design decision made by a specification rather than by a
debate.

### Scope

This document defines the **artifact**, not the interoperation surface. A second
implementer learns from it what a debate record means and can validate one, but
not which endpoints to serve, what the authentication header is, what the request
field names are, what the error semantics are, or what the SSE event names are.
Those names are not even uniform today: a vote is `support_agent_id` in the REST
request body, `support_id` on a message record, and `supports_id` on a vote
record. Specifying the interoperation surface is a separate piece of work,
tracked as issue #37, and until it exists the claim "a second implementation can
be built against this" is limited to producing and reading artifacts.

The MCP transport still has no export tool. That debt belongs to the same
risk-matrix row and stays tracked (#35); this decision is about defining the
protocol, not about adding a transport surface, and an MCP tool is a
public-contract change with its own trigger.

## Alternatives considered

- **Write SPEC.md as documentation, without a checker.** Rejected as the failure
  the issue was reframed to avoid. Prose cannot be conformed to, and prose
  drifts silently.
- **Generate SPEC.md from the checker.** Rejected because the descriptive half —
  the state model, the degradations, the non-guarantees — is what a reader
  actually needs, and it cannot be derived from assertions.
- **Treat the implementation as the specification and ship only tests.**
  Rejected because a second implementation would have to read Go to learn the
  protocol, which is the same as having no protocol.
- **Assert the rules directly in the existing golden test.** Rejected because the
  rules would then apply only to checked-in fixtures. Conformance has to be
  callable on bytes from elsewhere, or it tests this repository rather than the
  protocol.
- **Check rules against service internals rather than the artifact.** Rejected:
  it would be inapplicable to any other implementation and would couple the suite
  to storage.
- **Freeze more rules now (contiguous `seq`, mandatory verdict, timestamp
  ordering).** Rejected because each states something the implementation does not
  guarantee. A normative rule that the reference implementation violates under a
  known degradation is worse than no rule.

## Rollback criterion

The rules published here become a public contract at the moment another party
builds against them, and narrowing one afterwards is a compatibility break. The
falsifiable criterion is therefore about the rules being wrong, not about the
tests being red.

Roll back — by a follow-up ADR that removes or narrows the specific rule — if
either holds:

1. A rule fires on an artifact produced by a **correct** run of the reference
   implementation. That is a rule stating something the protocol does not
   guarantee, and the export it rejects is evidence.
2. A documented degradation (moderator unavailable, budget exhausted, a write
   failing between the verdict and the state) cannot produce a conforming
   artifact. The degradations are intended behaviour; a rule that outlaws them is
   the defect.

Every rule was falsified before acceptance: each has a mutation test proving the
check rejects a violation, and C9, C10, and C13 were additionally falsified by
breaking the service — dropping the embargo, letting the model override the
votes, and inventing votes for participants who never spoke — and confirming the
suite fails.

### The criterion is currently weaker than it reads

Criterion 1 says a rule firing on a correct run triggers rollback. Nothing runs
`conformance.Check` outside the test suite, so in operation nobody would see it
fire. What the suite does check is better than the fixtures alone suggest —
`TestExportedArtifactConformsToSpec` drives a real HTTP export against a
file-backed store with the production clock, `crypto/rand` identifiers and
genuinely staggered join times — but every artifact ever checked is still
produced by this repository's own tests.

The obvious experiment, running `Check` over an artifact from the live service,
could not be performed: the export endpoint is not yet deployed there, and the
only debate present is an unstarted smoke test. That is worth stating rather than
omitting, because it means the fifteen rules have never met a real moderator, a
real deadline expiry, or a real restart.

Two consequences follow. First, the criterion is honest but latent until an
export from a production debate is checked; doing so once after the next deploy
is the cheapest evidence available and costs nothing. Second, a standing signal —
checking exports in the handler behind a flag and counting violations by rule
identifier — is tracked as issue #38, and until it exists criterion 1 depends on
someone deliberately looking.

Adding a rule after publication is a compatibility change of the same weight as
removing one: an artifact that conformed may stop conforming. The test for which
one an amendment is must be observable rather than a statement of intent:

> An amendment that rejects no artifact the previous rule accepted is an edit.
> An amendment that rejects any artifact the previous rule accepted is a
> compatibility change and requires its own ADR — whether it is framed as a new
> rule or as closing a gap in an old one.

An earlier draft exempted amendments that "close a gap in a rule already
published". That exemption was removed because it turns on the author's framing,
and framing is exactly what this decision exists to stop being the arbiter: the
first person wanting a new constraint would describe it as gap-closing and
nothing in the document could contradict them. The C11 widening in this change is
the worked example — extending it from `open` to every not-yet-started status
rejects artifacts that previously conformed, so after publication it would need
an ADR. It was free here only because nothing is published yet.

A rule also needs its own rejection case per branch once a branch becomes the
sole enforcement of a distinct sentence in its SPEC body. C10 crossed that line
and now has cases for both anti-forgery branches; C5's four claims have three
cases between them for the same reason.

If the SPEC-to-checker equality test is ever suppressed or made advisory, this
decision has lost its mechanism and requires a follow-up ADR rather than a
comment.

## Consequences

- The artifact has a document a second implementation can produce and validate
  against, and a suite that decides whether it succeeded. The interoperation
  surface remains unspecified (#37).
- No normative rule can be added, dropped, or left unenforced without failing
  `make check`, and no published bound can drift from the constant behind it.
  Drift inside a rule body remains a review obligation, not an executable one.
- The risk-matrix row for REST/MCP parity and the exported artifact loses its
  conformance debt and keeps its MCP-export and execution-metadata debt.
- Adding a normative rule is now a versioning decision, not an edit. This is the
  intended cost.
- Six reference traces must be regenerated whenever a compatible schema change
  lands, through `make golden` rather than by hand.
