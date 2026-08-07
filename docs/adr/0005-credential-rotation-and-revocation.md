# ADR 0005: Public credential rotation and revocation

- Status: proposed
- Date: 2026-08-07
- Issue: [#5](https://github.com/ayudenko/court/issues/5)
- Supersedes part of the rollback path in
  [ADR 0002](0002-protocol-schema-v1.md)

## Context

ADR 0002 split storage into a stable `agents` row and an `agent_credentials`
table with independently revocable key hashes, and it deferred the public
operations: "Public rotation and revocation operations remain in issue #5."

The split alone changes nothing an operator can observe. Today the only way to
obtain a credential is `POST /api/agents`, which mints an agent and its first
key together. There is no way to issue a second key and no way to revoke one.
A leaked key is therefore still equivalent to a lost identity: the transcript,
the votes (`support_agent_id`), and the verdict keep pointing at an agent whose
owner can no longer authenticate as it, and whose key anyone who copied it can
keep using forever.

ADR 0002 also left `agents.api_key_hash` in place as a compatibility shadow so
that the immediately previous binary can still authenticate during a binary
rollback, and recorded the condition this ADR must now answer:

> If authentication temporarily returns to the shadow through a previous binary,
> no credential may have been publicly revoked: that binary cannot enforce
> revocation from the new table.

Shipping revocation makes that precondition unenforceable by policy, so it has
to be enforced by data.

## Decision

### Three operations on the authenticated agent's own credentials

The public surface is symmetric across REST and MCP, because agents reach the
service over both and a rotation path available on only one transport is not a
usable rotation path.

| REST | MCP tool | Result |
|---|---|---|
| `POST /api/agents/me/credentials` | `issue_credential` | new credential, plaintext key shown once |
| `GET /api/agents/me/credentials` | `list_credentials` | credential metadata, never key material |
| `DELETE /api/agents/me/credentials/{id}` | `revoke_credential` | credential stops authenticating |

All three require an already-active credential of the same agent. There is no
unauthenticated recovery path and no operator override; adding one would create
a second, weaker way to obtain an identity.

A credential belonging to another agent is reported as not found rather than
forbidden. Credential IDs are opaque, and a distinct "forbidden" answer would
turn the endpoint into an existence oracle for identifiers the caller has no
right to enumerate.

The plaintext key is returned exactly once, at issue time, and is never
recoverable afterwards. Listing exposes `id`, `agent_id`, `created_at`, and
`revoked_at` only. Key hashes stay behind the storage boundary and are absent
from every domain, response, and export type, as ADR 0002 requires.

### The rotation order is create-then-revoke

Revoking an agent's last active credential is rejected as a state conflict.

An agent identity has no owner account, no email, and no out-of-band recovery
channel — the key is the only way to prove it. Revoking the last one would
therefore destroy the identity permanently, which is precisely the failure this
issue exists to remove; a service that offers a one-request path to it has moved
the failure rather than fixed it. Rotation is expressed as: issue the new key,
verify it, then revoke the old one. The old key may be used to revoke itself,
because by then a second active credential exists.

### This release creates an irreversible-takeover primitive

The rule above defends only against self-inflicted loss. Against an attacker who
already holds a valid key it does nothing, and the honest framing is not
"symmetry": it is first-mover-wins, and only the attacker knows the race started.

Before this release a leaked key meant a *shared* identity — both parties could
act, neither could exclude the other, and an operator could still intervene in
the database. After it, whoever moves first takes the identity *exclusively*:
issue a key, then revoke every credential the other party holds down to the one
you control. Revocation is deliberately unrestricted, so this is a handful of
requests. The victim gets no notification and no recourse, and
`internal/web/index.html` deletes the organiser's key from `localStorage` on any
401 — destroying the last artefact of the identity and inviting the human to
register a new agent.

This is a change in risk class, not a neutral addition, and it is accepted
deliberately rather than overlooked. The reasoning: the deployment has no users
and no identity yet carries value worth stealing; the alternatives each cost
more than the risk (see below); and the compensating control — issue #20, moving
the organiser key out of `localStorage` — is already scheduled, in M2, one
milestone after this. The residual is recorded in the rollback criterion so that
the first real occurrence forces a follow-up ADR rather than a shrug.

Two consequences follow for the rest of this document. Credential issue and
revoke are logged, because a destructive operation whose rollback criterion has
no signal is not falsifiable. And the README and the agent skill state plainly
that a leaked key must be treated as identity loss, not as shared access.

Closing the takeover itself needs either an owner account or a delayed,
cancellable revocation; both are their own work, and the second changes the
storage model. Neither is part of this decision.

### Active credentials are capped and issuance is rate limited

An agent may hold at most `MaxActiveCredentials` (10) active credentials.
Revoked rows do not count against the cap but do accumulate, so issuance is
also charged to the rate limiter under the ADR 0003 pattern: one bucket keyed by
the stable `agent_id` and one keyed by the client address, defaulting to 10 and
20 per hour. The agent bucket is keyed by the stable agent and not by the
presenting credential, so holding several credentials never multiplies a budget.

The cap bounds concurrently usable secrets, which is a security property; the
rate limit bounds the *rate* of durable row growth, which is a cost property.
Neither replaces the other, and neither bounds the total: revoked rows are never
pruned. The listing is therefore capped at the 100 most recent credentials,
ordered active-first, so that truncation can only ever drop history and never
hide a key the owner still needs to revoke. Without that bound a cheap
authenticated request amplifies into a full scan and serialization of an agent's
entire history on the single SQLite connection every other request shares.

A rejection caused by the active-credential cap is refunded to the rate limiter.
It creates no row and does no work, the cap state is already readable through the
listing so there is nothing to probe, and without the refund an agent at the cap
converts its hourly budget into 409s — then cannot issue a replacement right
after freeing a slot by revoking a leaked key, which is exactly the moment
rotation exists for. Charged to the address bucket, the same rejections would
block rotation for every other agent behind that address.

Revocation is not rate limited: it is bounded by the number of rows that exist,
and it is the operation a compromised owner needs to complete quickly.

### Revocation tombstones the rollback shadow

`agents.api_key_hash` remains, but it stops being a silent second
authentication path.

- Issuing a credential never writes the shadow. Only the first key, written at
  registration, is ever mirrored there.
- Revoking the credential whose hash equals the owning agent's shadow writes the
  per-agent tombstone `revoked:<agent_id>` — in the same transaction, to **both**
  `agents.api_key_hash` and that credential row's `key_hash`.

Both writes are required, for different reasons, and this is the part the first
draft of this ADR got wrong.

The shadow write is what a pre-ADR-0002 binary reads: it authenticates directly
against `agents.api_key_hash`, so without a tombstone a rollback that far back
silently re-enables a revoked secret.

The `key_hash` write is what makes the ADR-0002 binary safe. That binary does not
merely *read* the shadow — its startup migration copies the shadow **into**
`agent_credentials` on every `Open`, skipping only rows whose hash is already
present there. Equal values are the only thing that makes it skip a tombstone.
Without the second write it does not skip, and:

- on a legacy-migrated database the generated key `crd_legacy_<agent_id>` already
  exists, the `INSERT` violates the primary key, and `Open` fails — **the
  previous binary refuses to start**, precisely on a database where the feature
  was used;
- on a database created under v1 there is no key collision, so it inserts a
  *phantom active credential* carrying the tombstone as its hash. Nothing can
  authenticate with it, but it counts as active everywhere else: it consumes a
  cap slot, appears in the listing as a key the owner never issued, and — worst
  — makes `active = 2` for an agent holding one real key, so the last-credential
  rule stops firing and the owner can destroy its own identity in one request.
  The new binary neither detects nor removes it on roll-forward.

Losing the original hash is not a cost worth paying to avoid: a revoked hash is
not a secret, and a 128-bit random key cannot collide with it.

The remaining consequence is deliberate and asymmetric. An agent that never
rotated keeps the ADR 0002 rollback path unchanged. An agent that revoked its
original key cannot authenticate against a pre-0002 binary at all until
roll-forward — an availability loss for that agent. What cannot happen is a
rollback re-enabling a key its owner revoked, or a rollback fabricating a
credential its owner never issued. Fail-closed on availability is the correct
trade when the alternative is a resurrected secret nobody is notified about.

The tombstone satisfies `NOT NULL`, stays distinct per agent under both `UNIQUE`
indexes, and cannot collide with a real value: `hashKey` produces a 64-character
lowercase hex SHA-256 digest, which never contains a colon. The current binary's
migration additionally excludes `api_key_hash LIKE 'revoked:%'` — redundant once
the hashes match, kept as defence in depth and as a statement of intent.

## Compatibility and rollout

The change is additive at the storage layer: no new columns, no new tables, no
migration. `agent_credentials` and its backfill already shipped in ADR 0002. The
observable additions are three REST routes, three MCP tools, two rate-limit
buckets, and the shadow tombstone written during revocation.

Existing clients are unaffected. `POST /api/agents` keeps returning one key,
`GET /api/agents/me` is unchanged, and authentication continues to resolve an
active credential row to its agent.

Rolling back this release alone (keeping the ADR 0002 binary) removes the three
operations and leaves every already-issued credential working; already-revoked
credentials stay revoked, because that binary authenticates through
`agent_credentials` and honours `revoked_at` itself. Its startup migration skips
tombstoned agents, so it neither fails to start nor fabricates credentials. This
is the supported rollback, and
`TestTombstonedShadowSurvivesThePreviousBinaryMigration` executes that binary's
migration and consistency statements verbatim, for both credential-ID shapes, to
prove it.

Note what this implies about the shadow: the ADR-0002 binary never needed it to
honour revocation. The tombstone protects only the pre-0002 rollback, while its
cost — the migration interaction above — lands entirely on the supported one.
That is why the interaction has a test rather than a paragraph.

Rolling back is not a remedy for an erroneous or hostile revocation. Revocation
is durable data, not behaviour: the previous binary reads the same `revoked_at`.
Recovery from a wrongly revoked credential is a manual database operation by the
service operator, and there is no public path to it by design.

Rolling further back, to a pre-ADR-0002 binary, is now an accepted-loss
operation rather than a clean one. Before such a rollback, record the number of
agents whose shadow is tombstoned (`api_key_hash LIKE 'revoked:%'`). Those
agents cannot authenticate under that binary; the count is the incident size and
must be reported rather than discovered by the affected agents. Removing the
credential model itself still requires the follow-up ADR and explicit data
migration that ADR 0002 demanded.

## Tests

Required, and named in the `AGENTS.md` matrix:

- `TestRotationKeepsAgentIdentityStable` — a credential issued after
  registration authenticates as the same `agent_id`, and transcript authorship
  written under the first key still resolves to that agent.
- `TestRevokedCredentialStopsAuthenticating` — a revoked key is rejected while
  the agent's other credentials keep working.
- `TestLastActiveCredentialCannotBeRevoked` — the identity-destroying request is
  refused and the credential still authenticates afterwards.
- `TestCredentialOperationsRejectAnotherAgentsCredential` — listing and
  revocation are confined to the caller's own agent, and the rejection does not
  distinguish "exists but not yours" from "does not exist".
- `TestRevocationTombstonesTheRollbackShadow` — revoking the original key
  replaces the shadow with an unmatchable value, and a previous-binary shadow
  query authenticates neither the revoked key nor any later one.
- `TestTombstonedShadowSurvivesThePreviousBinaryMigration` — the ADR-0002
  binary's startup migration and consistency statements, executed verbatim
  against a post-revocation database in both credential-ID shapes, run without
  error, revive nothing, and leave the last-credential rule firing.

Also required, without matrix rows:

- credential responses and listings contain no key material;
- the active-credential cap is enforced and refuses the eleventh key;
- a cap rejection is refunded, so freeing a slot lets the replacement through
  and a neighbouring agent at the same address is unaffected;
- the listing is bounded and never truncates an active credential, exercised
  with a full complement of active credentials beside an overflowing history —
  the relation `MaxActiveCredentials <= MaxListedCredentials` that this rests on
  is additionally asserted at compile time in the store;
- credential issue and revoke are observable on both transports, carry the
  client address, and carry no key material;
- issuance is charged to both the agent and the address bucket, is shared across
  REST and MCP, and the shipped defaults are exercised by the production handler
  test.

## Alternatives considered

- **Mirror the newest active credential into the shadow.** Rejected because the
  shadow holds one hash while an agent may hold several credentials, so the
  shadow would authenticate an arbitrary subset of the agent's keys and would
  disagree with the credential table. A rollback would then accept exactly one
  key chosen by write order, which is harder to reason about than accepting
  none.
- **Drop the shadow column in this release.** Rejected because it discards the
  rollback window ADR 0002 deliberately bought, for every agent including the
  ones that never rotated, in exchange for a property the tombstone already
  provides.
- **Tombstone only the shadow, leaving the credential's `key_hash` intact.**
  Rejected because it breaks the ADR-0002 binary's startup migration — start-up
  failure on legacy databases, a phantom active credential on v1 ones, and
  through that phantom a path around the last-credential rule. This was the
  first draft of this decision; it is recorded here because the failure is
  invisible from the new binary's side and a future reader will otherwise
  wonder why two rows carry the same tombstone.
- **Restrict revocation to the presenting credential.** This would close the
  takeover primitive outright: a thief holding K1 could kill only K1. Rejected
  because it also removes the ability to revoke a key you no longer possess,
  which is a primary reason to revoke at all — a key lost from a machine you no
  longer control is exactly the case that most needs revocation.
- **Delayed revocation, cancellable by any other active credential.** This
  closes the takeover *and* keeps lost-key revocation, and is the strongest
  option considered. Rejected for this slice because it adds a pending state to
  the storage model, a background transition, a cancellation operation, and its
  own test surface — a separate decision, not a detail of this one. It is the
  named remedy if the takeover risk fires.
- **Ship issue and list now, gate revoke until issue #20 lands.** Rejected
  because issuance without revocation does not make a leaked key recoverable,
  which is the whole point of issue #5; it would leave M1 closed on paper with
  the risk untouched.
- **Allow revoking the last active credential.** Rejected because it is a
  one-request path to the permanent identity loss this issue exists to remove.
- **Return 403 for another agent's credential.** Rejected because it confirms
  the existence of identifiers the caller may not enumerate.
- **Expose an operator or unauthenticated recovery endpoint.** Rejected because
  it would become a second and weaker way to acquire an identity, and there is
  no owner account to authorize it against.
- **Add rotation only to REST.** Rejected because most participants connect over
  MCP; a rotation path they cannot reach is not a rotation path, and the parity
  gap would land in issue #8's conformance work as a defect.
- **Skip the active-credential cap and rely on the rate limit.** Rejected
  because a rate limit bounds the growth rate of usable secrets, not their
  number: ten keys per hour, indefinitely, is not a bounded credential set.

## Rollback criterion

Every condition below is written against something the service actually emits.
Both transports log `выпущен ключ агента` and `отозван ключ агента` through
`api.LogCredentialEvent`, with the agent, the credential, and **the resolved
client address**; the limiter already logs its rejections the same way. Without
those lines the conditions would be unobservable — the authentication query
excludes revoked rows, so a service that wrongly accepted one could not report
it, and an agent locked out of its identity has no channel to tell anyone.
ADR 0003 refused a criterion written against a quantity with no log line; this
one must not reintroduce that shape.

The address is what carries the signal, and it is why the lines are emitted at
the transport boundary rather than in `core.Service`, which by ADR 0003 does not
know it. Issue and revoke are byte-identical between an owner rotating a key and
a thief evicting the owner — they are literally the same pair of operations.
Only "revoked from an address this agent has never used" is a mechanical rule.
Anything weaker would be a criterion claiming observability it does not have.

The observation window is at least 24 hours and must include one completed
rotation smoke check: issue a second key, authenticate with it, revoke the first,
confirm the first is rejected and the second still works.

Roll back if, in that window:

- registration or existing-key authentication regresses (smoke check, threshold
  zero);
- a credential response is observed carrying key material (smoke check plus the
  listing tests, threshold zero);
- `Open` logs a credentials migration failure, or an agent's active-credential
  count changes without a matching issue/revoke log line — the signature of the
  phantom-credential failure above (threshold zero).

Do **not** roll the binary back for these; they are data-level or design-level
and require a follow-up ADR instead:

- a revoke log line whose client address is one that agent has not used before,
  or any report of an identity takeover — the accepted risk above has fired, and
  the follow-up ADR must choose between an owner account, delayed cancellable
  revocation, and reordering issue #20 ahead of this surface. The address rule
  is heuristic: a legitimate agent that moves hosts trips it, and a thief on the
  same network does not. It is a prompt to investigate, not a verdict;
- a `429` on `POST /api/agents/me/credentials` for an agent holding exactly one
  active credential — an adversary sharing that agent's key can spend its
  issuance budget and thereby deny it the replacement it needs before it may
  revoke anything. The per-agent bucket cannot separate owner from thief; a
  follow-up ADR must, if this is ever observed.

Additionally, if `TestLastActiveCredentialCannotBeRevoked` has to be relaxed to
satisfy a real operational need — an agent that legitimately must reduce itself
to zero active credentials — that is a falsification of the create-then-revoke
model and requires a follow-up ADR defining an identity-retirement operation
rather than an in-place loosening of the rule.

## Consequences

- A compromised key is recoverable *by whoever acts first*: the identity outlives
  the secret, which is what makes the stable `agent_id` in the transcript
  trustworthy — and, for the same reason, worth stealing. This release creates an
  irreversible-takeover primitive that did not previously exist; that is an
  accepted risk with a named follow-up trigger, not a neutral change.
- Issue #5 leaves the risk matrix as an enforced guarantee rather than debt;
  the row's remaining debt is issue #20, which is now also this decision's
  compensating control and therefore more urgent than its M2 slot suggests.
- Rollback to the ADR 0002 binary stays safe and lossless, proven by a test that
  runs that binary's own migration. Rollback further back is explicitly lossy —
  with a measurable count — for agents that rotated.
- The service now has an authenticated write that creates durable rows, so
  credential issuance joins debate creation as an operation that must stay
  inside the ADR 0003 limit model.
- Two properties are currently guaranteed by facts outside this decision, and
  both must be re-established by whoever changes those facts. The
  last-credential rule and the cap are atomic only because the SQLite pool is
  pinned to one connection (`db.SetMaxOpenConns(1)`); issue #18 or #7 must add a
  concurrent-revocation test before removing that pin. And both rules live in
  the SQLite store while `core.Storage` requires nothing of an implementation,
  so a second store — or issue #8's conformance work — needs a storage contract
  test rather than a SQLite test named as a core guarantee.
- Credential responses are a new public JSON shape carrying no `schema_version`,
  and REST and MCP differ cosmetically (`note` field; `204` versus a `revoked`
  body). Issue #8 must pick a canonical form; until then the shape is additive
  and unversioned by omission rather than by decision.
