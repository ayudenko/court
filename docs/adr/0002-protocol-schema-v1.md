# ADR 0002: Versioned protocol, export, moderation, and credential schemas

- Status: accepted
- Date: 2026-08-06
- Issue: [#16](https://github.com/ayudenko/court/issues/16)

## Context

The public SSE stream currently serializes `core.Event` without a version. The
same `Message` shape is the only durable protocol record. Structured moderator
results exist while a round is being moderated, but the store keeps only their
rendered text, so claims, citations, decisions, and unresolved questions cannot
be reproduced by a future export.

Agent identity and authentication are also coupled in storage: `agents` owns a
single `api_key_hash`. This prevents multiple independently revocable
credentials and makes a compromised key equivalent to a lost identity.

Issue #17 must be able to record deterministic golden traces, and issue #11
must be able to export a debate without inventing a format or reconstructing
structured data from prose. Those consumers require a versioned schema first.

## Decision

### Public and durable versions have independent authorities

Public protocol version 1 covers live events and JSONL export records.
`schema_version` is the integer `1` on every serialized event and every JSONL
record. Durable moderation payloads also start at version 1, but their version
authority is storage-local and does not share the public current-version
constant. A future public v2 must continue decoding durable moderation v1; a
non-additive durable change gets its own dispatch and migration without
implicitly changing SSE or export.

Within a version, fields may be added and consumers must ignore fields they do
not understand. A field may not be removed, renamed, change type, or change
meaning. Any such change requires a new schema version and an explicit
compatibility path. JSON timestamps are UTC RFC 3339 values. Stable agent and
debate IDs remain opaque strings; transcript citations refer to the debate's
monotonically increasing message `seq` values.

Enum and discriminator additions are not field additions. A new event name,
export `record_type`, message `kind`, debate mode, or debate status requires a
new schema version. Version-1 producers reject unknown tags and any explicit
schema version other than 1; their serialization boundary supplies version 1
when the caller leaves it unset and normalizes timestamps to UTC.

### Live events stay additive and flat

The existing event names and fields remain unchanged. `schema_version` is
added to the current flat JSON object rather than introducing a new envelope,
so existing SSE consumers continue to work. Serialization supplies version 1
even when internal code constructs an event without setting the field and
rejects unsupported explicit versions.

Supported version-1 consumers are the built-in web client and REST, MCP, and
SSE JSON consumers that follow the protocol rule to ignore unknown object
fields. A consumer configured to reject every unknown JSON field is not
compatible with additive version-1 evolution and is not a supported decoder.

### Export is a typed JSONL record stream

The canonical export schema is a tagged record with `schema_version`,
`record_type`, and `debate_id`, followed by exactly one typed payload. Version 1
defines records for debate metadata, participants, messages, current votes,
round summaries, and the final verdict. Participant and moderation metadata
that the service does not yet collect are optional; absence is different from
an empty measured value.

The mapping from stored transcript kind to export record is one-to-one:
`argument` and `system` become `message`; `summary` becomes `round_summary`;
`verdict` becomes `verdict`. A pre-v1 summary or verdict retains its ordered
text with an absent typed `result`, explicitly representing unavailable legacy
evidence. It is never downgraded to a plain message or reconstructed from prose.
New writes of `summary` or `verdict` must contain exactly their matching typed
payload; storage rejects missing, mismatched, dual, or payload-on-argument
forms. When storage reads a pre-v1 prose-only row it sets an internal legacy
marker. The export mapper applies the same consistency checks and allows an
absent result only with that marker; direct unmarked or contradictory values
are rejected.

The export record's JSON serializer is the canonical producer boundary: it
adds version 1 when omitted, validates that exactly one typed payload matches
the tag, rejects non-v1 schemas and tags, and emits all timestamps in UTC.
The canonical stream order is exactly one debate record, participants sorted
by opaque `agent_id`, all transcript records sorted by `seq`, then current votes
sorted by `agent_id`. Duplicate participant IDs, transcript sequence numbers,
or vote agent IDs are invalid. `protocol.CanonicalStream` is the shared
assembler for export and golden-trace producers and rejects records for another
debate or records placed in the wrong group.

Issue #16 defines and tests these types. Issue #11 will expose them through an
HTTP endpoint, while issue #17 will generate golden traces in the same format.
No export endpoint or recorded trace is part of this decision.

### Structured moderation is durable

Messages retain their rendered `text` for existing clients and gain an
optional typed `round_summary` or `verdict` payload. The store adds one nullable
JSON payload column. Its durable envelope contains its own `schema_version`
plus exactly one typed `round_summary` or `verdict`, which must agree with the
message kind. New moderator summaries and verdicts store both representations.
Existing rows remain valid with no structured payload; their prose is not
parsed or guessed. An unsupported durable version fails closed rather than
being decoded into current types. Durable v1 is represented by immutable
storage-local DTOs and explicit conversions to current core types; it does not
reuse public/core JSON tags. Complete literal v1 fixtures cover every summary
and verdict field and are tested independently from a simulated future
public-protocol version.

The deterministic hybrid verdict also uses the typed verdict shape, so all new
verdicts can be exported consistently even when no LLM is configured. In
hybrid mode both intermediate-summary and final-verdict consensus values are
overwritten from participant-vote state, so a provider cannot contradict the
mode's consensus authority.

Moderator citations are checked against the authoritative `seq` values loaded
from stored message records. Header-looking text inside a participant's message
is untrusted prompt content and never creates a valid citation target.
If persistence of a typed summary or verdict fails, the service leaves the
debate in `moderating`, does not advance or conclude it, and publishes neither
the next-turn nor conclusion event. Operator recovery can therefore act
without durable state falsely claiming that missing evidence was recorded.
If the typed message succeeds but the following debate-state write fails,
restart recovery reuses the single stored result for that round instead of
calling the moderator or appending it again. Multiple typed summaries or
verdicts for one round fail closed as ambiguous. A transcript reread failure
after persisting a summary also blocks verdict production and conclusion until
recovery can load the complete authoritative transcript.

### Credentials are separate from stable agents

The target storage model is:

- `agents`: stable identity and public metadata;
- `agent_credentials`: credential ID, owning agent ID, unique key hash,
  creation time, and optional revocation time.

Authentication resolves an active credential to its agent and never uses the
agent row as the identity secret. Registration creates the agent and its first
credential atomically. Credential hashes are never serialized into public
domain or export types.

Opening a legacy database creates `agent_credentials` and idempotently copies
each existing hash while preserving the agent ID and key. For rollback, the
`agents.api_key_hash` column remains as a compatibility shadow in both migrated
and fresh version-1 databases. Registration writes the initial hash to the
shadow and the authoritative credential row atomically; authentication in new
code uses only active credential rows. This allows the immediately previous
binary to authenticate and register during a binary-only rollback. Public
rotation and revocation operations remain in issue #5.

## Compatibility and rollout

The rollout is additive:

1. create and backfill the credentials table in one startup migration while
   retaining the rollback shadow;
2. authenticate only through active credential rows;
3. keep new registrations compatible with the previous binary's credential
   query and insert statements;
4. add structured message payload storage without rewriting old messages;
5. emit versioned events while preserving existing names and fields.

A binary rollback requires a moderation quiescence window: stop new debate
writes, wait until no debate is `moderating`, and only then start the previous
binary. While that binary is connected to a v1 database, debate mutation and
moderation traffic stays blocked; health and credential rollback checks may run.
Record the count of prose-only `summary`/`verdict` rows (`moderation_json IS
NULL`) before rollback and again before roll-forward. The delta must be zero.
A nonzero delta is an explicit accepted-loss incident: each row remains
exportable as marked legacy prose, but its claims and citations cannot be
recovered. The previous-binary insert/roll-forward test proves such rows are
counted and surfaced rather than silently treated as typed evidence.

Tests must cover event versioning, structured moderation round trips, legacy
credential migration, preservation of existing authentication, multiple
credentials per stable agent, revocation rejection, migration idempotence,
previous-binary credential queries, unsupported versions and tags, and UTC
normalization. Canonical-stream tests use permuted inputs and require bytewise
identical JSONL ordering. Fault-injection tests require structured-message
write failures to block round and conclusion transitions, and require a
post-message state-write failure plus restart to yield exactly one typed result
without a duplicate message event. A separate read-failure test blocks verdict
production when the post-summary transcript cannot be reloaded. The legacy
migration fixture uses the complete pre-v1 storage
shape, including foreign keys and indexes, and verifies every representative
non-default agent, debate, participant, and transcript field before and after a
second open. A normalized schema fingerprint compares every legacy column
attribute, foreign key, index definition, and AUTOINCREMENT property before and
after both opens, excluding only the intentional v1 additions.

## Alternatives considered

- **Share one current-version constant between public and durable schemas.**
  Rejected because a public-only v2 would make already persisted moderation v1
  unreadable, while a storage-only evolution would unnecessarily change SSE.
- **Version only the future export.** Rejected because SSE and export would no
  longer describe one protocol and live consumers could not declare which
  semantics they received.
- **Replace the event object with a nested envelope.** Rejected because it
  breaks existing SSE clients without providing a version-1 benefit.
- **Reconstruct structured moderation from rendered text.** Rejected because
  parsing prose silently loses claims and citations and recreates the marker
  parsing failure fixed by issue #9.
- **Store summaries and verdicts only in separate tables.** Rejected for this
  slice because ordering them with arguments would still require a message
  reference; an optional typed message payload preserves the existing ordered
  protocol with less migration surface.
- **Remove the hash shadow from fresh databases.** Rejected because a database
  created by this release could not be opened by the previous binary during a
  routine rollback. The compatibility shadow can be removed only with a tested
  downgrade migration or after binary rollback is no longer required.
- **Keep one key hash on each agent.** Rejected because it cannot represent
  independently revocable credentials and keeps identity coupled to a secret.

## Rollback criterion

`TestEveryEventKindSerializesCurrentSchemaVersion` is the supported-decoder
compatibility canary, and
`TestWriteSSEMakesProtocolSerializationRejectionObservable` proves rejected
production is surfaced instead of silently dropping bytes. Protocol rejection
uses the distinct `SSE protocol: отклонено событие` error signal; client
disconnects and other writer failures use `SSE transport: запись события` and
do not trigger schema rollback. Replay, live-event, and heartbeat writes all
use that classification, terminate the affected stream on failure, and have
failing-writer tests enforcing the separation.

The deployment observation window lasts at least 24 hours and until the deploy
smoke check completes one full built-in-web debate while observing join, start,
turn, message, conclusion, and replay events, whichever is later. Before golden
traces are published, the protocol-rejection threshold is zero; any occurrence
triggers rollback. A replay read/decode failure is separately logged and closes
the stream rather than presenting incomplete history as a successful replay.
Roll back as well if the smoke flow fails, or if the complete pre-v1 migration
test changes an agent ID, rejects its valid key, loses or changes a
debate/participant/transcript field, schema fingerprint, or rendered text.
Rollback is also invalid if moderation cannot be quiesced or if the
prose-only-moderation inventory increases; do not claim lossless roll-forward
in that case.

If authentication temporarily returns to the shadow through a previous binary,
no credential may have been publicly revoked: that binary cannot enforce
revocation from the new table. Once public revocation ships, removal or rollback
of the credential model requires a follow-up ADR and an explicit data migration.

## Consequences

- Export and golden-trace work inherit one concrete versioned schema.
- Existing REST, MCP, web, and SSE behavior remains compatible.
- Structured moderator evidence is reproducible instead of being trapped in
  prose.
- Stable agent identity can outlive individual credentials.
- Version-1 databases temporarily retain an authentication-unused secret-hash
  shadow until a later reviewed cleanup or downgrade migration.
