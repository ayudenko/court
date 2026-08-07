// Package golden records deterministic debate scenarios in the versioned
// protocol JSONL format and replays them through the canonical schema boundary.
package golden

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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

const replayLineLimit = 1024 * 1024

var fixedTime = time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

// Artifact is one checked-in golden JSONL trace.
type Artifact struct {
	Name string
	Data []byte
}

type scenario struct {
	name        string
	mode        core.DebateMode
	rounds      int
	question    string
	description string
	arguments   [2]string
	moderator   scriptedModerator
}

var scenarios = []scenario{
	{
		name:        "moderator_consensus_v1.jsonl",
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
		name:        "hybrid_split_vote_v1.jsonl",
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
		data, err := MarshalJSONL(records)
		if err != nil {
			return nil, fmt.Errorf("encode %s: %w", scenario.name, err)
		}
		artifacts = append(artifacts, Artifact{Name: scenario.name, Data: data})
	}
	return artifacts, nil
}

// ReplayJSONL validates a trace and returns its records in canonical order.
// Re-encoding the result produces the stable representation used by fixtures.
func ReplayJSONL(data []byte) ([]protocol.ExportRecord, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), replayLineLimit)
	var records []protocol.ExportRecord
	for line := 1; scanner.Scan(); line++ {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			return nil, fmt.Errorf("line %d: blank JSONL record", line)
		}
		var record protocol.ExportRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, fmt.Errorf("line %d: decode: %w", line, err)
		}
		if err := record.Validate(); err != nil {
			return nil, fmt.Errorf("line %d: validate: %w", line, err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read JSONL: %w", err)
	}
	if len(records) == 0 {
		return nil, errors.New("trace contains no records")
	}

	debate := records[0]
	var participants, transcript, votes []protocol.ExportRecord
	for index, record := range records[1:] {
		switch record.RecordType {
		case protocol.RecordParticipant:
			participants = append(participants, record)
		case protocol.RecordMessage, protocol.RecordRoundSummary, protocol.RecordVerdict:
			transcript = append(transcript, record)
		case protocol.RecordVote:
			votes = append(votes, record)
		default:
			return nil, fmt.Errorf("record %d: unexpected type %q after debate", index+2, record.RecordType)
		}
	}
	return protocol.CanonicalStream(debate, participants, transcript, votes)
}

// MarshalJSONL serializes validated records as one JSON object per line.
func MarshalJSONL(records []protocol.ExportRecord) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	for index, record := range records {
		if err := encoder.Encode(record); err != nil {
			return nil, fmt.Errorf("record %d: %w", index+1, err)
		}
	}
	return output.Bytes(), nil
}

func recordScenario(spec scenario) (_ []protocol.ExportRecord, err error) {
	database, err := store.Open(":memory:")
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	defer func() {
		err = errors.Join(err, database.Close())
	}()

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
	})
	if err != nil {
		return nil, fmt.Errorf("create debate: %w", err)
	}
	if _, err := service.JoinDebate(challenger, created.ID, "verify"); err != nil {
		return nil, fmt.Errorf("join debate: %w", err)
	}
	if _, err := service.StartDebate(creator, created.ID); err != nil {
		return nil, fmt.Errorf("start debate: %w", err)
	}

	events := service.Subscribe(created.ID)
	defer service.Unsubscribe(created.ID, events)
	if _, err := service.PostArgument(context.Background(), creator, created.ID, spec.arguments[0], ""); err != nil {
		return nil, fmt.Errorf("creator argument: %w", err)
	}
	if _, err := service.PostArgument(context.Background(), challenger, created.ID, spec.arguments[1], ""); err != nil {
		return nil, fmt.Errorf("challenger argument: %w", err)
	}
	if err := waitForConclusion(events); err != nil {
		return nil, err
	}

	view, err := service.GetDebate(created.ID)
	if err != nil {
		return nil, fmt.Errorf("read concluded debate: %w", err)
	}
	if view.Status != core.StatusConcluded {
		return nil, fmt.Errorf("status = %q, want %q", view.Status, core.StatusConcluded)
	}
	messages, err := service.Messages(created.ID, 0)
	if err != nil {
		return nil, fmt.Errorf("read transcript: %w", err)
	}
	return recordsFor(view, []core.Agent{creator, challenger}, messages)
}

func recordsFor(
	view core.DebateView,
	agents []core.Agent,
	messages []core.Message,
) ([]protocol.ExportRecord, error) {
	debate := protocol.ExportRecord{
		RecordType: protocol.RecordDebate,
		DebateID:   view.ID,
		Debate: &protocol.DebateRecord{
			Question: view.Question, Description: view.Description, Mode: view.Mode, Status: view.Status,
			Rounds: view.Rounds, CurrentRound: view.CurrentRound, TurnTimeoutSec: view.TurnTimeout,
			PrepTimeSec: view.PrepTime, CreatorID: view.CreatorID, Consensus: view.Consensus,
			CreatedAt: view.CreatedAt,
		},
	}
	agentsByID := make(map[string]core.Agent, len(agents))
	for _, agent := range agents {
		agentsByID[agent.ID] = agent
	}
	participants := make([]protocol.ExportRecord, 0, len(view.Participants))
	for _, participant := range view.Participants {
		agent, ok := agentsByID[participant.AgentID]
		if !ok {
			return nil, fmt.Errorf("participant %q has no agent metadata", participant.AgentID)
		}
		participants = append(participants, protocol.ExportRecord{
			RecordType: protocol.RecordParticipant,
			DebateID:   view.ID,
			Participant: &protocol.ParticipantRecord{
				AgentID: participant.AgentID, Name: participant.Name, Persona: agent.Persona,
				Stance: participant.Stance, JoinedAt: participant.JoinedAt,
			},
		})
	}
	transcript := make([]protocol.ExportRecord, 0, len(messages))
	for _, message := range messages {
		record, err := protocol.RecordForMessage(message)
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", message.Seq, err)
		}
		transcript = append(transcript, record)
	}
	votes := make([]protocol.ExportRecord, 0, len(view.Votes))
	for _, vote := range view.Votes {
		votes = append(votes, protocol.ExportRecord{
			RecordType: protocol.RecordVote,
			DebateID:   view.ID,
			Vote: &protocol.VoteRecord{
				AgentID: vote.AgentID, AgentName: vote.AgentName,
				SupportsID: vote.SupportsID, SupportsName: vote.SupportsName,
			},
		})
	}
	return protocol.CanonicalStream(debate, participants, transcript, votes)
}

func waitForConclusion(events <-chan core.Event) error {
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			if event.Type == core.EventConcluded {
				return nil
			}
		case <-timer.C:
			return errors.New("scenario did not conclude")
		}
	}
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
) (core.RoundSummary, error) {
	return core.RoundSummary{
		Summary: "The fixture inputs and protocol invariants are reproducible.",
		Claims: []core.ModerationClaim{{
			Text: "Both participants require deterministic evidence.", Citations: slices.Clone(allowedSeqs),
		}},
		UnresolvedQuestions: []string{},
		Decisions:           []string{"Regenerate fixtures through one command."},
		Consensus:           moderator.consensus,
	}, nil
}

func (moderator scriptedModerator) Summary(
	ctx context.Context,
	question string,
	transcript string,
	round int,
	allowedSeqs []int64,
) (core.RoundSummary, error) {
	return moderator.CheckRound(ctx, question, transcript, round, allowedSeqs)
}

func (moderator scriptedModerator) Verdict(
	_ context.Context,
	_ string,
	_ string,
	allowedSeqs []int64,
) (core.ModerationVerdict, error) {
	return core.ModerationVerdict{
		FinalAnswer: "Use deterministic record/replay fixtures as protocol evidence.",
		Claims: []core.ModerationClaim{{
			Text: "The result is supported by the recorded arguments.", Citations: slices.Clone(allowedSeqs),
		}},
		UnresolvedQuestions: []string{},
		Decisions:           []string{"Keep record and replay on the canonical v1 schema."},
		Consensus:           moderator.consensus,
	}, nil
}
