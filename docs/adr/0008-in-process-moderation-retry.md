# ADR 0008: In-process retry for stuck moderation

- Status: accepted
- Date: 2026-08-08
- Issue: [#40](https://github.com/ayudenko/court/issues/40)
- Amends the unaccounted-spend bound recorded in
  [ADR 0004](0004-moderator-spend-ceiling.md)

## Context

Moderation deliberately refuses to conclude a debate it cannot explain. When the
write of a verdict, of a degradation notice, or of the concluding state
transition fails, `moderate` returns and leaves the debate in `moderating`
(ADR 0002: "If persistence of a typed summary or verdict fails, the service
leaves the debate in `moderating`"). That choice is right: a transcript missing
the record that explains the outcome is indistinguishable from a truncated one.

What was wrong is what happened next. The only retry was `Service.recover`,
called once from `Run` at process start. `expireTurns` handles `preparing` and
`running` and never looked at `moderating`. On a server that is not restarted,
such a debate hung indefinitely: the turn belongs to nobody, there is no
deadline, and every participant is blocked. No conformance rule reports it,
because C7 does not require a verdict — the export of a hung debate is valid.

Two facts made this worth fixing now rather than later. Issue #36 landed
degradation notices in every mode, so a `hybrid` deployment with no LLM key
writes a notice in *every* debate; the number of writes that can fail on the
path that must not conclude silently went up. And issue #10 is about to make
that same keyless `hybrid` path the default showcase.

## Decision

### The background ticker resumes moderation, and `recover` is gone

`Run` calls `resumeStuckModeration` at startup and on every tick, alongside
`expireTurns`. It reads the active debates and hands every one in `moderating`
to `moderateAsync`. At startup the in-memory state is empty, so the first sweep
does exactly what `recover` did; from then on the same code keeps doing it, and
logs `модерация: возобновляю зависший проход` the way `recover` used to.

`moderateAsync` is now the **only** launch point for `moderate` — both the round
boundary in `advanceTurn` and the sweep go through it — so "a pass is already
running" is one counter rather than an assumption held separately by two paths.

A pass in flight is excluded by a `running` flag rather than by a timer, so the
retry delay never has to be reasoned about against `moderationTimeout`. The flag
is cleared by the pass's own deferred call, matched on a `pass` token so a late
goroutine from a previous round cannot unmark a live pass of the next one. The
one thing that overrides the flag is a *newer round closing*: for a debate to
reach that point the previous pass must already have finished its work, and the
flag survives only until its deferred call runs. Without the override a launch
landing in that window would be dropped, which costs one tick on a server running
the sweep and costs everything where nothing runs it — the golden-trace recorder
builds a `Service` and never calls `Run`. A
call whose round is *below* the recorded one is refused outright: the sweep reads
the debate list without the state-transition lock, so its snapshot can be stale,
and starting on a stale snapshot would run a second pass on top of a live one.
`moderate` re-checks the status as a last line: a pass that starts after the
debate has left `moderating` returns without touching it.

### What is bounded is the cost of retrying, not the retrying

This is the decision the first draft of this ADR got wrong, and it is the centre
of the design. Retries are **not** capped. A retry that does not call the model
costs nothing, and most do not: a transcript reread that failed after a summary
was stored, a conclude write that failed with the verdict already stored, a
keyless `hybrid` deployment, a plain read error. Capping those would strand a
debate on any storage fault outlasting the cap — reintroducing issue #40 in a
narrower window, in the mode that is about to become the showcase.

What is capped is the model call. `admitModeration` now answers three questions
where `moderationAllowed` answered one:

- **Is there budget left?** — the per-debate ceiling of ADR 0004, unchanged.
- **Has this round already been admitted for `moderationMaxPaidPasses` calls?** —
  a per-round retry backstop. Which of the two binds first depends on call size,
  not on configuration: at the shipped 500 000-token ceiling a short debate's call
  costs ~10k, so ten admissions arrive long before the ceiling does, while a
  debate at the validation limits exhausts the ceiling in two. So a stuck short
  debate publishes `unreachable` with `reason=paid_cap` in the log, and a stuck
  large one publishes `budget`. It counts *admissions*, not charges: a call
  that reaches the provider and returns nothing is charged nothing (ADR 0004
  classifies it as unbilled), but the provider may still have executed it, and a
  counter that only counts charges would repeat such a call forever.
- **Is the accounting still working?** — see below.

A "no" to any of them does not stop the pass. It sends the pass down the
degradation path with the cause that is true: an exhausted ceiling reports
budget, and the other two report that the moderator was not used. Liveness is
therefore never traded for money; the debate always ends up either concluded or
retrying for free.

Retrying is not free of I/O, though: every pass reads the whole transcript, and
the storage failure that causes retries usually stalls every writing debate at
once. So the pause between passes doubles up to a fifteen-minute cap. That keeps
recovery from a short fault fast and a long outage cheap, without capping the
number of passes.

A retry is also bound by what its round has already published. A pass that
recorded a degradation notice and then failed its next write leaves that notice
in the transcript; the retry arrives a minute later, by which time the provider
may answer again. Writing the record the notice denies would make the artifact
say two things about one round — `SPEC.md` promises each notice identifies what
was lost and whether a record follows — and no conformance rule catches it,
because both statements are individually legal. So a recorded notice binds its
round: the retry reproduces the outcome the notice promised instead of the one
the recovered provider would now allow.

One state is exempt, because retrying it is provably useless: a round with two
typed summaries or two typed verdicts is rejected as ambiguous by
`moderationMessagesForRound` and cannot be moderated by this process or any
later one. It is marked and not retried, so the error line that reports it stays
one line rather than one per pause.

### A charge that cannot be persisted stops the spending

`chargeModeration` increments `Debate.ModeratorTokens` only when
`Storage.AddModeratorTokens` succeeded. When it fails, the debate is marked as
having lost its accounting and no further pass will pay for a model call.

Without this the retry loop and the spend ceiling would cancel each other out. A
storage failure that drops the verdict write typically drops the accounting write
too — a full volume, a read-only remount, a locked database do not distinguish an
`INSERT` from an `UPDATE`. Every retry would then re-read a spend counter that
never moved, find the full budget available, and pay for another call, with the
ceiling reporting that nothing had been spent. That is ADR 0004's own rollback
criterion 2 fired by the mechanism meant to improve liveness.

The alternative of remembering the lost *amount* and adding it to the ceiling's
arithmetic was implemented and then rejected: it is the first piece of service
state not derivable from storage, it can double-count if a store ever returns an
error after committing, and it bounds the damage less tightly than simply
refusing to spend. Refusing costs one unaccounted call per debate per process —
the same shape as the bound ADR 0004 already accepts for a process that dies
mid-call, and composed the same way, since the ceiling is per debate. The
user-visible half of the price is larger than the money half and should be stated
plainly: one transient accounting failure makes every remaining round of that
debate deterministic for the rest of the process, because the mark is never
cleared.

To the transcript a refusal to pay presents as **the moderator being
unavailable**, not as an exhausted budget. This is the one place where an earlier
draft of this ADR was wrong and the repository said so in three voices: SPEC.md
publishes the six notices with an explicit `Cause` column whose values are
`unreachable` and `budget`; `degradationCause` states in the code that `degraded`
"обязан значить «сработал потолок расхода»"; and
`TestUnavailableModeratorIsNotReportedAsBudgetDegradation` is an enforced test
whose whole subject is refusing that conflation one layer up. A debate at four
percent of its ceiling that publishes "the moderator budget for this debate is
exhausted" is a false claim about the operator's spend, and it is a claim a
consumer cannot check, because `ModeratorTokens` is not in the artifact.

Of the two published causes, `unreachable` is the one no stored record
contradicts. Neither is exact — under a lost charge the moderator is
demonstrably reachable, since the service called it successfully at least once,
and in a multi-round debate model-authored summaries can sit directly above the
notice. But `budget` is falsifiable against `debates.moderator_tokens`, which
would read near zero, and `unreachable` is falsifiable against nothing in the
artifact. Publishing the unfalsifiable-but-inexact claim is the lesser fault, and
SPEC.md now states the widened meaning — "the moderator was not used" — rather
than leaving the reader to infer it. So the paid-cap and lost-accounting refusals
write the unavailability notices and take the unavailability outcome — in `moderator` mode that means concluding with no
verdict, in `hybrid` a verdict counted from the votes. The operator's finer
question is answered in the log, where `charge_lost` distinguishes a real
provider failure from a self-imposed refusal, and `attribution` says whether the
line's degradation was observed by this pass or recovered from the transcript by
a later one. The residual is that an artifact consumer counting `unreachable` as
provider health now double-counts storage faults, and nothing in the artifact
resolves it for them. If `учёт расхода модератора` ever recurs across debates,
the question to reopen is whether two published causes are enough — not the
accounting mechanism.

The same rule decides one more thing in `hybrid`. ADR 0004 forbids quoting the
tally leader's own words into a deterministic `final_answer` when a participant
could have chosen the trigger, because exhausting a budget with long arguments is
something any participant can do. Refusing to pay is not such a trigger: reaching
it requires breaking a storage write, which takes the whole service down rather
than winning one debate. So it keeps the quote, as the provider-failure path
already does.

### The degradation attribution survives a resumed conclusion

The `расход модератора за дебаты` line — the one ADR 0004 counts criterion 1
from — moved after the successful concluding write, so a pass that fails to
conclude no longer emits it and a retry cannot multiply it. That created a
second-order problem: the retry takes the already-stored verdict and degrades
nothing itself, so it would report `degraded=false` for a debate whose ceiling
had fired. The attribution is therefore re-derived from the transcript: a
degraded verdict is spoken by `система` and is preceded by the notice naming its
cause. The speaker check is required because the moderator-mode unavailability
notice is also written where no verdict follows, and a later pass may then add a
model verdict beside it.

That makes the speaker name load-bearing, so `courtd` now refuses to start with
`COURT_MODERATOR_NAME` equal to it. ADR 0004 had already noted that an operator
choosing that name "erases even that distinction"; this change turns the note
into a startup error.

### The retry policy is a decision of the service, not of the deployment

`moderationRetryDelay` is one minute and `moderationMaxPaidPasses` is ten per
round. Both are fields with those constants as defaults, overridable only from a
`_test.go` file. There is deliberately no environment variable: an operator who
could set the paid cap to zero would silently turn every debate into a
deterministic one, and one who could set the delay to zero would turn a storage
outage into a hot loop.

## What this changes in ADR 0004

ADR 0004's "What this does not bound" says of unaccounted spend: *"This is one
unaccounted call per crash, not a loop, because the retry is charged normally."*
That sentence described a world where the retry happened only at process start.
Read against this ADR it becomes:

- **Mid-call process death is unchanged.** One call, charged to nobody, then a
  normally charged retry.
- **A stuck round now retries inside the process, indefinitely, but its model
  calls are still admitted one at a time by the ceiling.** Every admitted call is
  charged normally; the first call whose charge cannot be persisted is the last
  one the debate pays for. So the unaccounted-spend bound stays at one call per
  debate — now per process rather than per crash — and the loop is a loop of free
  work.
- **ADR 0004's other unaccounted case is bounded by admission counting, not by
  charging.** ADR 0004 records that a billed response lost after the model
  finished is classified as never-arrived and charged nothing, "up to one such
  call per round boundary". A retry loop counting only charges would turn that
  into one per pause, indefinitely; counting admissions instead keeps it at
  `moderationMaxPaidPasses` per round.
- **A restart resets the paid-pass backstop and the lost-accounting mark, not
  the ceiling.** The ceiling lives in storage and an `auto_stop` cycle does not
  touch it. With accounting working, the composed figure is unchanged from
  ADR 0004. With accounting broken, each idle cycle costs one more admitted call
  per round before the mark is set again.

The two sentences in ADR 0004 that describe the old mechanism
(`recover` re-running moderation, and `chargeModeration` updating its in-memory
copy before incrementing the row) are now inaccurate as descriptions of the code.
They are left in place because ADR 0004 is a dated decision; the header of this
ADR is the breadcrumb, and issue #41 tracks the general problem of ADR prose that
outlives its code.

## Alternatives considered

- **Persist a `moderating_since` timestamp and drive the retry from it.**
  Rejected: it changes the storage model — its own ADR trigger and its own
  migration — to obtain a fact the process already knows about work it is itself
  doing. The in-memory record is also strictly more correct, because it can tell
  "a pass is running here" from "a pass ran somewhere at some time".
- **Reuse `TurnDeadline` as the retry clock.** It is `json:"-"` and invisible in
  the export, so it looked free. Rejected: `TurnStatus.DeadlineSec` publishes it
  unconditionally, so a debate in `moderating` would start advertising a deadline
  to agents polling for their turn, and the field would carry two meanings.
- **Cap the number of retries and leave the debate for an operator afterwards.**
  This was the first draft of this ADR. Rejected: the cap is charged before the
  pass runs, so it is spent by retries that cost nothing, and any storage outage
  longer than the cap window strands the debate exactly as issue #40 described.
  Capping the paid calls instead bounds the same money and strands nothing.
- **Remember the amount of a charge that could not be persisted.** Rejected —
  see above.
- **Retry on the ticker with no delay, or with a constant one.** Rejected: in
  `moderator` mode a pass admitted by the ceiling re-asks the model, so a
  two-second cadence would spend the whole debate budget in under a minute. A
  constant minute is affordable in money but not in I/O — a permanently stuck
  debate would read its whole transcript 1440 times a day, on the disk that is
  already the thing that failed. Doubling with a cap costs a few extra minutes of
  recovery latency and nothing else.

## Rollback

Revert the commit. Nothing durable changes: no schema, no wire format, no
storage model, no public API. The previous behaviour — retry only at process
start — returns exactly, including its bug. The one deployed-behaviour change a
revert also undoes is the accounting refusal, which is why it is recorded here
rather than left as an implementation detail.

## Rollback criterion

Two observables, both greppable in the service's own log, both fatal to a claim
this ADR makes:

- **`модерация: неоднозначные сохранённые результаты` or
  `гибрид: неоднозначные сохранённые результаты`, once.** Both literals are
  quoted because the two modes emit different prefixes
  and `hybrid` is the one issue #10 is about to make default. The line means
  `moderationMessagesForRound` found two summaries or two verdicts in one round,
  which can only happen if two passes ran concurrently — and after it, the debate
  cannot be moderated at all, by this process or any later one. That is worse
  than the bug this ADR fixes, so the threshold is one occurrence. A second
  `debate_concluded` event is the same failure seen from outside, but it is
  deliberately **not** used as the criterion: `Hub.Publish` neither logs nor
  persists, and drops events when no subscriber is listening, so nobody would
  see it.
- **`расход модератора` for one debate whose `debate_total` exceeds `budget`,
  once.** Admission uses a byte-based upper bound, so a charged call should never
  carry the debate past its ceiling; if one does, the ceiling is admitting calls
  the retry loop then pays for. Strictly *exceeds*, not "reaches": a call
  admitted exactly at the boundary whose provider reports no usage is charged its
  own upper bound and lands on the ceiling legitimately, so an inclusive
  threshold would fire on a correct run — ADR 0007's criterion-1 failure shape.
  This needs no reconciliation against provider billing, which is what made
  ADR 0004's own criterion 2 hard to act on.

Neither `модерация: повторяю зависший проход` nor `учёт расхода модератора` is a
rollback criterion — but both are the operational alarms, and this ADR removed
the only other one when it dropped the attempt cap. `модерация: повторяю зависший
проход` recurring for one debate is how an operator learns that a debate is
wedged; `модерация: платный вызов модели невозможен` says the service has stopped
paying for it and why. `учёт расхода модератора` recurring across many debates is
the signal that would justify revisiting the rejected "remember the amount"
alternative — though note that deleting a debate mid-pass also produces it once,
so a single line is not that signal.

## Consequences

`moderating` becomes a transient state on a running server rather than one that
waits for a restart, and `SPEC.md` says so — descriptively, since no rule on an
artifact can check liveness. The two tests that used to prove dedup by building
a second `Service` over the same database now run one service, which is what the
property was always supposed to be about.

A debate whose storage never recovers now retries forever instead of hanging
silently. Each pass reads the debate and its whole transcript, so the cost is a
full transcript read per stuck debate per pause, and the pause grows to fifteen
minutes. That is the trade this ADR takes: the failing state is loud and
self-healing rather than quiet and permanent.

The reserved-name refusal is a small compatibility edge: a database written by an
earlier binary configured with that moderator name holds model verdicts the new
attribution reads as service-authored. ADR 0004 had already declared such a
deployment as having erased the distinction, so nothing is newly broken.
