package core_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"court/internal/core"
	"court/internal/store"
)

type consensusModerator struct{}

func (consensusModerator) Name() string { return "test moderator" }

func (consensusModerator) CheckRound(
	context.Context,
	string,
	string,
	int,
) (core.RoundSummary, error) {
	return core.RoundSummary{
		Summary:   "The tested invariants hold.",
		Decisions: []string{"Conclude the debate."},
		Consensus: true,
	}, nil
}

func (consensusModerator) Summary(
	context.Context,
	string,
	string,
	int,
) (core.RoundSummary, error) {
	return core.RoundSummary{}, errors.New("unexpected hybrid summary")
}

func (consensusModerator) Verdict(
	context.Context,
	string,
	string,
) (core.ModerationVerdict, error) {
	return core.ModerationVerdict{
		FinalAnswer: "Consensus reached.",
		Decisions:   []string{"Keep the agreed position."},
		Consensus:   true,
	}, nil
}

func TestDebateStateMachineReachesConsensus(t *testing.T) {
	service := newTestService(t)
	creator := registerAgent(t, service, "creator")
	challenger := registerAgent(t, service, "challenger")

	created, err := service.CreateDebate(creator, core.CreateDebateParams{
		Question:       "Which approach should we use?",
		Description:    "Visible only after the debate starts.",
		Mode:           core.ModeModerator,
		Rounds:         3,
		TurnTimeoutSec: core.MinTurnTimeout,
	})
	if err != nil {
		t.Fatalf("CreateDebate: %v", err)
	}
	if created.Status != core.StatusOpen || created.CurrentRound != 0 {
		t.Fatalf("created state = %s round %d", created.Status, created.CurrentRound)
	}
	if created.Description != "" {
		t.Fatal("description must stay hidden while the debate is open")
	}

	if _, err := service.JoinDebate(challenger, created.ID, "challenge the proposal"); err != nil {
		t.Fatalf("JoinDebate: %v", err)
	}
	started, err := service.StartDebate(creator, created.ID)
	if err != nil {
		t.Fatalf("StartDebate: %v", err)
	}
	if started.Status != core.StatusRunning || started.CurrentRound != 1 {
		t.Fatalf("started state = %s round %d", started.Status, started.CurrentRound)
	}
	if started.TurnAgentID != creator.ID {
		t.Fatalf("first turn = %q, want %q", started.TurnAgentID, creator.ID)
	}
	if started.Description == "" {
		t.Fatal("description must be visible after the debate starts")
	}

	if _, err := service.PostArgument(context.Background(), challenger, created.ID, "too early", ""); !errors.Is(err, core.ErrNotYourTurn) {
		t.Fatalf("out-of-turn PostArgument error = %v, want ErrNotYourTurn", err)
	}
	if _, err := service.PostArgument(context.Background(), creator, created.ID, "initial position", ""); err != nil {
		t.Fatalf("creator PostArgument: %v", err)
	}
	afterFirst, err := service.GetDebate(created.ID)
	if err != nil {
		t.Fatalf("GetDebate after first argument: %v", err)
	}
	if afterFirst.TurnAgentID != challenger.ID || afterFirst.CurrentRound != 1 {
		t.Fatalf("second turn = %q round %d", afterFirst.TurnAgentID, afterFirst.CurrentRound)
	}
	if _, err := service.PostArgument(context.Background(), challenger, created.ID, "revised position", ""); err != nil {
		t.Fatalf("challenger PostArgument: %v", err)
	}

	concluded := waitForStatus(t, service, created.ID, core.StatusConcluded)
	if !concluded.Consensus || concluded.CurrentRound != 1 || concluded.TurnAgentID != "" {
		t.Fatalf("concluded debate = consensus:%t round:%d turn:%q", concluded.Consensus, concluded.CurrentRound, concluded.TurnAgentID)
	}

	messages, err := service.Messages(created.ID, 0)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	wantKinds := []string{core.KindArgument, core.KindArgument, core.KindSummary, core.KindVerdict}
	if len(messages) != len(wantKinds) {
		t.Fatalf("message count = %d, want %d", len(messages), len(wantKinds))
	}
	for i, message := range messages {
		if message.Kind != wantKinds[i] {
			t.Fatalf("message %d kind = %q, want %q", i, message.Kind, wantKinds[i])
		}
		if i > 0 && message.Seq <= messages[i-1].Seq {
			t.Fatalf("message seq is not monotonic: %d after %d", message.Seq, messages[i-1].Seq)
		}
	}
}

func TestAuthenticateRejectsUnknownKey(t *testing.T) {
	service := newTestService(t)
	if _, err := service.Authenticate("unknown-key"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("Authenticate error = %v, want ErrUnauthorized", err)
	}
}

func newTestService(t *testing.T) *core.Service {
	t.Helper()
	database, err := store.Open(filepath.Join(t.TempDir(), "court.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return core.NewService(database, core.NewHub(), consensusModerator{}, logger)
}

func registerAgent(t *testing.T, service *core.Service, name string) core.Agent {
	t.Helper()
	agent, _, err := service.RegisterAgent(name, "test participant")
	if err != nil {
		t.Fatalf("RegisterAgent(%q): %v", name, err)
	}
	return agent
}

func waitForStatus(t *testing.T, service *core.Service, debateID string, want core.DebateStatus) core.DebateView {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		debate, err := service.GetDebate(debateID)
		if err != nil {
			t.Fatalf("GetDebate: %v", err)
		}
		if debate.Status == want {
			return debate
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("debate did not reach status %q", want)
	return core.DebateView{}
}
