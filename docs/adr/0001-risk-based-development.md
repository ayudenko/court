# ADR 0001: Risk-based development contract

- Status: accepted
- Date: 2026-08-06
- Source: court debate [`dbt_d4a827317251`](https://court.ayudenko.by/d/dbt_d4a827317251)

## Context

court is a small Go service maintained by one human working with several AI
agents. Code generation is cheap; trustworthy verification and retained system
understanding are the constraints. Before this decision the core state machine
had no tests, the repository had no CI, and the roadmap placed scenario tests
after format-dependent golden traces.

A fixed roster of architect, developer, tester, and reviewer agents would add
handoff cost without adding independent evidence. At the same time, using one
context for every decision creates confirmation bias around protocol, security,
storage, and public-contract changes.

## Decision

### Verification follows risk

Tests protect invariants whose failure is silent, broad, or expensive to undo.
The initial checks are:

- a core-only debate scenario from creation through consensus;
- turn-order and monotonic transcript-sequence invariants;
- rejection of unknown credentials;
- existing structured-moderation checks.

Scenario work from issue #14 may run in parallel with #9 → #16 → #17 only
while it uses core Go types and the `Storage`/`Moderator` interfaces. It must not
contain serialized fixtures, wire-field literals, or snapshots. Format and
golden-trace checks remain after #9 and #16. The
`scenario-boundary-check` make target mechanically rejects known transport and
JSON imports plus conventional snapshot and golden-trace dependencies from core
test files. Arbitrary serialized literals cannot be recognized reliably by a
textual gate; preventing those remains an explicit independent-review
obligation. `matrix-check` fails if a test named as enforced in `AGENTS.md`
differs from the canonical `quality/enforced-tests.txt` manifest or is not
discovered by `go test -list` in its declared package.

### One executable quality gate

`make check` is the canonical local and CI command. It checks formatting, runs
`go vet`, and runs the tests. `AGENTS.md`, CI, and human documentation refer to
make targets rather than duplicating command sequences.

The risk-to-check matrix is deliberately small. A row names an existing test;
if implementation is pending, it points to an issue and is explicitly debt,
not a guarantee.

### Context is split only for independent evidence

The agent holding the task context implements the change and its tests.
Separate agents are used for parallel read-only research or fresh-context
review, not as a permanent role-playing team.

The shared policy lives in `AGENTS.md`. Project-scoped adapters in
`.codex/agents/` and `.claude/agents/` express the same reviewer and researcher
roles in each tool's native format. These adapters inherit the parent model and
default to a read-only tool or sandbox configuration, so the policy does not
depend on a particular model release. For Codex, live parent permission
overrides take precedence over that sandbox default. Independent review
therefore cannot run from a permissive parent mode.

For design review, the author writes the ADR. The reviewer receives the
specification, diff, and ADR, but not the author's reasoning transcript. A
review objection states a risk, a failure scenario, and the fact or test that
would resolve it.

Codex review roles are spawned without parent history (`fork_turns="none"`) or
in a separate fresh task. Their prompt contains the complete staged diff,
relevant ADRs, current status, and validation output. This makes context
isolation an orchestration requirement instead of relying on a reviewer to
ignore author reasoning already present in its context.

### ADR triggers

An ADR is mandatory for changes to:

1. versioned schemas or serialization;
2. the public REST/MCP contract;
3. the storage model;
4. a trust boundary, including credentials, auth, and spend limits.

An ADR is also required outside those classes when rollback is expensive or
irreversible. Each ADR records alternatives and a falsifiable rollback
criterion. When that criterion fires, a follow-up ADR is required.

### Rules have a lifecycle

At every milestone, each risk-matrix rule is classified as:

- **worked** — it caught a failure or changed a review outcome;
- **automated** — a named make target or linter enforced by CI supersedes the
  prose rule;
- **tracked debt** — the risk remains and the row links an open implementation
  issue;
- **basis gone** — the underlying risk no longer exists.

Adding or removing a rule requires a PR and independent review. Historical
rationale remains in the PR instead of accumulating in `AGENTS.md`.

## Rollout

The first vertical slice lands together: `Makefile`, CI, at least one core-only
scenario from #14, and the roadmap exception that permits that scenario to run
in parallel. Further scenario, auth, store, and REST/MCP parity checks follow;
the matrix gains enforced rows only when those named tests exist.

## Alternatives considered

- **Keep the prior informal process.** Rejected because neither agents nor CI
  had an executable definition of done and the core state machine had no test.
- **Use a fixed architect/developer/tester/reviewer roster.** Rejected because
  role handoffs duplicate context without necessarily adding independent
  evidence.
- **Document rules without CI.** Rejected because prose cannot prove that a
  named check exists or runs before merge.
- **Delay every #14 scenario until golden traces.** Rejected because core-only
  scenarios can test domain invariants without freezing a wire format.

## Rollback criterion

Evaluate this contract at the first milestone containing at least five required
fresh-context reviews, or after two milestones, whichever comes first. A
follow-up ADR must narrow or replace the mandatory-review mechanism before the
next milestone if none of those reviews causes a P0-P2 finding or a change to
code, tests, or an ADR. An escaped P0/P1 incident in a covered risk area after
the required review and `make check` also triggers an immediate follow-up ADR
to change the review inputs, checks, or trigger list.

## Consequences

- Quality rules fail in CI instead of existing only as prose.
- Scenario tests can start before golden traces without freezing a wire format.
- Fresh-context review is reserved for changes where independence pays for its
  handoff cost.
- The process has explicit size limits (`AGENTS.md` ≤100 lines, matrix ≤6 rows)
  to reduce the risk of producing process instead of working tests.
