// Package golden records deterministic debate scenarios in the versioned
// protocol JSONL format and replays them through the canonical schema boundary.
package golden

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"court/internal/core"
	"court/internal/protocol"
	"court/internal/store"
)

var fixedTime = time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

// Artifact is one checked-in golden JSONL trace.
type Artifact struct {
	Name string
	Data []byte
}

// stage is the lifecycle point a scenario is recorded at. Traces of unfinished
// debates are recorded deliberately: an export is defined for every status, and
// SPEC.md needs observable evidence for the non-terminal ones rather than prose.
type stage int

const (
	// stageOpen records a debate that has not started, where the question is
	// public but the context is still withheld.
	stageOpen stage = iota
	// stagePreparing records the preparation phase, which is entered at start
	// and left on its own deadline. Recording it needs no clock control because
	// only leaving it does.
	stagePreparing
	// stageRunning records a debate mid-round, after one argument.
	stageRunning
	// stageConcluded records a debate that ran to its own end.
	stageConcluded
)

type scenario struct {
	name        string
	stage       stage
	mode        core.DebateMode
	rounds      int
	prepTime    int
	question    string
	description string
	arguments   [2]string
	moderator   scriptedModerator
}

var scenarios = []scenario{
	{
		name:        "open_embargo_v1.jsonl",
		stage:       stageOpen,
		mode:        core.ModeModerator,
		rounds:      3,
		question:    "Is the discussion context withheld until the debate starts?",
		description: "Withheld until start: a late joiner must not be able to read this by exporting the debate.",
		arguments: [2]string{
			"Never spoken — this debate has not started.",
			"Never spoken — this debate has not started.",
		},
		moderator: scriptedModerator{consensus: true},
	},
	{
		name:  "preparing_phase_v1.jsonl",
		stage: stagePreparing,
		// Hybrid, не moderator: ветка C11 «голосов нет и в подготовке» иначе
		// осталась бы без положительного артефакта — в режиме moderator голосов
		// не бывает вовсе, и трасса ничего бы о ней не говорила.
		mode:        core.ModeHybrid,
		rounds:      2,
		prepTime:    600,
		question:    "What does a debate look like while participants are still reading?",
		description: "Disclosed at start: the preparation phase is when this becomes readable.",
		arguments: [2]string{
			"Never spoken — turns have not begun.",
			"Never spoken — turns have not begun.",
		},
		moderator: scriptedModerator{consensus: true},
	},
	{
		name:        "hybrid_running_partial_v1.jsonl",
		stage:       stageRunning,
		mode:        core.ModeHybrid,
		rounds:      2,
		question:    "Is an unfinished debate a valid artifact?",
		description: "Exported mid-round: one participant has spoken and the round is not over.",
		arguments: [2]string{
			"A partial trace is what an observer of a stuck debate has to work with.",
			"Never spoken — the export happens before this turn.",
		},
		moderator: scriptedModerator{consensus: true},
	},
	{
		name:        "moderator_consensus_v1.jsonl",
		stage:       stageConcluded,
		mode:        core.ModeModerator,
		rounds:      3,
		question:    "Should protocol traces be reproducible?",
		description: "A conformance fixture must be cheap to regenerate after compatible schema changes.",
		arguments: [2]string{
			"Golden traces should come from the real state machine.",
			"Agreed, provided time, IDs, moderation, and ordering are deterministic.",
		},
		moderator: scriptedModerator{consensus: true},
	},
	{
		name:        "hybrid_multi_round_v1.jsonl",
		stage:       stageConcluded,
		mode:        core.ModeHybrid,
		rounds:      3,
		question:    "Does a debate that crosses a round boundary stay ordered?",
		description: "Three rounds: two of them close with a summary, the last with the verdict.",
		arguments: [2]string{
			"Round boundaries are where ordering rules stop being trivial.",
			"Then the reference traces have to contain one.",
		},
		moderator: scriptedModerator{consensus: false},
	},
	{
		name:        "hybrid_split_vote_v1.jsonl",
		stage:       stageConcluded,
		mode:        core.ModeHybrid,
		rounds:      1,
		question:    "Did the participants select one position?",
		description: "Each participant keeps their own position, so hybrid voting must report no consensus.",
		arguments: [2]string{
			"Keep the first position.",
			"Keep the second position.",
		},
		moderator: scriptedModerator{consensus: true},
	},
}

// Generate runs every scenario through core.Service and returns canonical
// JSONL artifacts without writing to disk.
func Generate() ([]Artifact, error) {
	artifacts := make([]Artifact, 0, len(scenarios))
	for _, scenario := range scenarios {
		records, err := recordScenario(scenario)
		if err != nil {
			return nil, fmt.Errorf("record %s: %w", scenario.name, err)
		}
		data, err := protocol.MarshalJSONL(records)
		if err != nil {
			return nil, fmt.Errorf("encode %s: %w", scenario.name, err)
		}
		artifacts = append(artifacts, Artifact{Name: scenario.name, Data: data})
	}
	return artifacts, nil
}

func recordScenario(spec scenario) (_ []protocol.ExportRecord, err error) {
	database, err := store.Open(":memory:")
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	defer func() {
		err = errors.Join(err, database.Close())
	}()

	// Подготовка кончается по дедлайну, а часы у рекордера заморожены: сценарий
	// с prepTime дальше stagePreparing не уедет, и первый же PostArgument
	// упрётся в ErrBadState. Ловушка для следующего сценария, а не для этих.
	if spec.prepTime > 0 && spec.stage != stagePreparing {
		return nil, fmt.Errorf("scenario %s sets prep time but records stage %d; "+
			"a frozen clock never ends preparation", spec.name, spec.stage)
	}

	ids := newDeterministicIDs(strings.TrimSuffix(spec.name, ".jsonl"))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := core.NewService(
		database,
		core.NewHub(),
		spec.moderator,
		logger,
		core.WithClock(func() time.Time { return fixedTime }),
		core.WithIDGenerator(ids.next),
	)

	creator, _, err := service.RegisterAgent("Architect", "Argues for reproducible protocol evidence.")
	if err != nil {
		return nil, fmt.Errorf("register creator: %w", err)
	}
	challenger, _, err := service.RegisterAgent("Reviewer", "Challenges silent nondeterminism.")
	if err != nil {
		return nil, fmt.Errorf("register challenger: %w", err)
	}
	created, err := service.CreateDebate(creator, core.CreateDebateParams{
		Question: spec.question, Description: spec.description, Stance: "record",
		Mode: spec.mode, Rounds: spec.rounds, TurnTimeoutSec: core.MinTurnTimeout,
		PrepTimeSec: spec.prepTime,
	})
	if err != nil {
		return nil, fmt.Errorf("create debate: %w", err)
	}
	if _, err := service.JoinDebate(challenger, created.ID, "verify"); err != nil {
		return nil, fmt.Errorf("join debate: %w", err)
	}

	wantStatus := core.StatusOpen
	if spec.stage > stageOpen {
		if _, err := service.StartDebate(creator, created.ID); err != nil {
			return nil, fmt.Errorf("start debate: %w", err)
		}
		wantStatus = core.StatusPreparing
	}
	if spec.stage > stagePreparing {
		wantStatus = core.StatusRunning
		if _, err := service.PostArgument(context.Background(), creator, created.ID, spec.arguments[0], ""); err != nil {
			return nil, fmt.Errorf("creator argument: %w", err)
		}
		if spec.stage == stageConcluded {
			texts := map[string]string{creator.ID: spec.arguments[0], challenger.ID: spec.arguments[1]}
			if err := runToConclusion(service, created.ID, []core.Agent{creator, challenger}, texts); err != nil {
				return nil, err
			}
			wantStatus = core.StatusConcluded
		}
	}

	// Трасса собирается тем же снимком и тем же продюсером, что и ответ
	// GET /api/debates/{id}/export. Иначе эталон подтверждал бы формат
	// генератора фикстур, а не формат сервера.
	snapshot, err := service.ExportSnapshot(context.Background(), created.ID)
	if err != nil {
		return nil, fmt.Errorf("read debate: %w", err)
	}
	if snapshot.Debate.Status != wantStatus {
		return nil, fmt.Errorf("status = %q, want %q", snapshot.Debate.Status, wantStatus)
	}
	return protocol.Stream(snapshot)
}

// runToConclusion speaks as whoever holds the turn until the debate ends, so a
// scenario spanning several rounds needs no script of its own. Moderation runs
// in its own goroutine, so the loop waits rather than assuming the next turn is
// already open. The recorded bytes stay deterministic regardless of that timing:
// turn order is fixed, and each agent always says the same thing.
func runToConclusion(service *core.Service, debateID string, agents []core.Agent, texts map[string]string) error {
	byID := make(map[string]core.Agent, len(agents))
	for _, agent := range agents {
		byID[agent.ID] = agent
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		view, err := service.GetDebate(debateID)
		if err != nil {
			return fmt.Errorf("read debate: %w", err)
		}
		if view.Status == core.StatusConcluded {
			return nil
		}
		if view.Status != core.StatusRunning || view.TurnAgentID == "" {
			time.Sleep(time.Millisecond) // moderation between rounds
			continue
		}
		speaker, ok := byID[view.TurnAgentID]
		if !ok {
			return fmt.Errorf("turn belongs to unknown agent %q", view.TurnAgentID)
		}
		if _, err := service.PostArgument(context.Background(), speaker, debateID, texts[speaker.ID], ""); err != nil {
			return fmt.Errorf("argument of %s: %w", speaker.Name, err)
		}
	}
	return errors.New("scenario did not conclude")
}

type deterministicIDs struct {
	scenario string
	counts   map[string]int
}

func newDeterministicIDs(scenario string) *deterministicIDs {
	return &deterministicIDs{scenario: scenario, counts: make(map[string]int)}
}

func (ids *deterministicIDs) next(prefix string) string {
	ids.counts[prefix]++
	return fmt.Sprintf("%s_%s_%02d", prefix, ids.scenario, ids.counts[prefix])
}

type scriptedModerator struct {
	consensus bool
}

func (scriptedModerator) Name() string { return "Fixture Moderator" }

func (moderator scriptedModerator) CheckRound(
	_ context.Context,
	_ string,
	_ string,
	_ int,
	allowedSeqs []int64,
) (core.RoundSummary, core.ModerationUsage, error) {
	// Расход не сообщается и вызов не помечен оплаченным: golden-трассы фиксируют
	// протокол, а не стоимость, поэтому бюджет здесь не расходуется вовсе.
	return core.RoundSummary{
		Summary: "The fixture inputs and protocol invariants are reproducible.",
		Claims: []core.ModerationClaim{{
			Text: "Both participants require deterministic evidence.", Citations: slices.Clone(allowedSeqs),
		}},
		UnresolvedQuestions: []string{},
		Decisions:           []string{"Regenerate fixtures through one command."},
		Consensus:           moderator.consensus,
	}, core.ModerationUsage{}, nil
}

func (moderator scriptedModerator) Summary(
	ctx context.Context,
	question string,
	transcript string,
	round int,
	allowedSeqs []int64,
) (core.RoundSummary, core.ModerationUsage, error) {
	return moderator.CheckRound(ctx, question, transcript, round, allowedSeqs)
}

func (moderator scriptedModerator) Verdict(
	_ context.Context,
	_ string,
	_ string,
	allowedSeqs []int64,
) (core.ModerationVerdict, core.ModerationUsage, error) {
	return core.ModerationVerdict{
		FinalAnswer: "Use deterministic record/replay fixtures as protocol evidence.",
		Claims: []core.ModerationClaim{{
			Text: "The result is supported by the recorded arguments.", Citations: slices.Clone(allowedSeqs),
		}},
		UnresolvedQuestions: []string{},
		Decisions:           []string{"Keep record and replay on the canonical v1 schema."},
		Consensus:           moderator.consensus,
	}, core.ModerationUsage{}, nil
}
