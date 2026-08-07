# Development contract

This repository optimizes for fast, reversible delivery with independent
verification where failure would be silent, broad, or expensive to undo.

## Commands

- `make check` — the required local and CI gate.
- `make scenario-boundary-check` — keep core scenarios independent of wire formats.
- `make test` — run all tests.
- `make test-race` — run tests with the race detector.
- `make fmt` — format Go sources.
- `make build` — build `courtd`.

Do not document raw Go command sequences elsewhere; link to these make targets.

## Definition of done

1. The change states its expected behavior and rollback path.
2. Tests cover the affected invariant, not implementation details or coverage
   percentage. Add a test when failure can be silent, broad, or irreversible.
3. `make check` passes.
4. A fresh-context review is required for protocol, credentials/auth, storage,
   trust-boundary, schema, public-contract, or expensive-to-reverse changes.
5. Documentation is updated only when code cannot make the rule obvious.

## Risk-to-check matrix

| Risk | Required check or tracked work | Status |
|---|---|---|
| Turn/round state machine | `TestDebateStateMachineReachesConsensus` | enforced by `make check` |
| HTTP trust boundary: auth and abuse limits | `TestAuthenticateRejectsUnknownKey`, `TestRegisterRateLimitRejectsBurstFromOneClient`, `TestCreateDebateRateLimitIsPerAgentKey`, `TestStreamLimitReleasesSlotOnDisconnect`, `TestShippedDefaultsAreEnforcedByTheProductionHandler` | enforced by `make check` |
| Structured moderation | `TestCheckRoundUsesStructuredResult`, `TestCheckRoundRejectsInvalidStructuredResult` | enforced by `make check` |
| Store concurrency/restart | issues #18 and #6 | tracked; named tests required when implemented |
| REST/MCP core-state parity | issue #8 | tracked; named conformance test required |
| Moderator spend boundary and credentials | `TestModeratorSpendCeilingDegradesToDeterministicVerdict`, `TestModeratorSpendCeilingKeepsConsensusFoundBeforeExhaustion`, `TestModeratorSpendCeilingSurvivesRestart`, `TestModeratorSpendCeilingChargesUnreportedUsageAsEstimate`, `TestModeratorSpendCeilingChargesFailedCalls`, `TestModeratorSpendCeilingDoesNotChargeUnbilledCalls`, `TestModeratorSpendCeilingRecordsOneNoticePerRoundAcrossRetries`, `TestUsageTravelsWithModerationErrors`, `TestFixedPromptBytesFitTheBudgetReserve`, `TestAnthropicCallToolReportsUsage`, `TestShippedModeratorBudgetIsEnforcedByTheProductionService` | spend enforced by `make check`; credentials (#5) and web key storage (#20) remain tracked debt |

A row without a named test must point to an issue and is debt, not an enforced
guarantee. `quality/enforced-tests.txt` maps every enforced name above to its Go
package; `make matrix-check` verifies the two sources match and that Go discovers
each test. Keep this table at six rows or fewer.

The six-row cap forces some rows to carry both an enforced guarantee and residual
debt. Such a row is classified per part, not as a whole: the named tests are
**automated**, and each linked issue stays **tracked debt** until it has its own
named test. A row loses its debt half only when no issue remains on it.

## Agent collaboration

- Keep implementation and its tests in one context for local, reversible work.
- Use subagents only for independent read-only research or genuinely
  independent review; do not create a fixed architect/developer/tester roster.
- Project roles live in `.codex/agents/` and `.claude/agents/`; keep their
  review intent aligned when either definition changes.
- After code, test, build, CI, or configuration changes, run `code_reviewer`
  (Codex) or `code-reviewer` (Claude) before completion.
- Also run the matching security reviewer for trust-boundary changes and the
  matching adversarial reviewer for ADR-triggering decisions.
- Launch Codex review roles with `fork_turns="none"`; if that control is not
  available, use a separate fresh task. Never launch independent review from a
  permissive parent permission mode because live overrides supersede the
  agent's read-only sandbox default.
- Use multiple `researcher` agents in parallel only when their read-only scopes
  are independent; the main agent owns synthesis and all edits.
- Provide reviewers only the specification, status, complete staged diff,
  relevant ADRs, and validation output—not the author's reasoning transcript.
- A review objection must name the risk, failure scenario, and fact or test that
  would resolve it.

## ADR triggers and rule lifecycle

Write an ADR before changing versioned schemas/serialization, the public
REST/MCP contract, the storage model, or a trust boundary. Also write one for
any decision with an expensive or irreversible rollback. Every ADR includes
alternatives and a falsifiable rollback criterion; triggering that criterion
requires a follow-up ADR.

At each milestone, classify every matrix row as: **worked** (link the finding),
**automated** (name the CI-enforced make target or linter), **tracked debt**
(link the still-open issue and show why the risk remains), or **basis gone**
(show the changed fact). Additions and removals use a PR and independent review;
history belongs in the PR, not in this file.
