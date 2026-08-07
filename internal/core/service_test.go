package core_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"court/internal/core"
	"court/internal/store"
)

type consensusModerator struct{}

type unavailableVerdictModerator struct{ consensusModerator }

type consensusSummaryModerator struct{ consensusModerator }

type unresolvedConsensusModerator struct{ consensusModerator }

type blockingConsensusModerator struct {
	consensusModerator
	started     chan struct{}
	release     chan struct{}
	startedOnce sync.Once
	releaseOnce sync.Once
}

type countingModerator struct {
	mu             sync.Mutex
	checkConsensus bool
	verdictErr     bool
	// usage — расход, который фейк сообщает на каждый вызов. Нулевое значение
	// означает «вызов не вошёл в счёт», и сервис не списывает за него ничего;
	// «ответ без расхода» — это Billed: true с нулевыми токенами.
	usage     core.ModerationUsage
	checks    int
	summaries int
	verdicts  int
}

func (*countingModerator) Name() string { return "counting moderator" }

func (m *countingModerator) CheckRound(
	context.Context, string, string, int, []int64,
) (core.RoundSummary, core.ModerationUsage, error) {
	m.mu.Lock()
	m.checks++
	m.mu.Unlock()
	return core.RoundSummary{Summary: "Counted summary.", Consensus: m.checkConsensus}, m.usage, nil
}

func (m *countingModerator) Summary(
	context.Context, string, string, int, []int64,
) (core.RoundSummary, core.ModerationUsage, error) {
	m.mu.Lock()
	m.summaries++
	m.mu.Unlock()
	return core.RoundSummary{Summary: "Counted hybrid summary.", Consensus: false}, m.usage, nil
}

func (m *countingModerator) Verdict(
	context.Context, string, string, []int64,
) (core.ModerationVerdict, core.ModerationUsage, error) {
	m.mu.Lock()
	m.verdicts++
	m.mu.Unlock()
	if m.verdictErr {
		return core.ModerationVerdict{}, m.usage, errors.New("injected moderator fallback")
	}
	return core.ModerationVerdict{FinalAnswer: "Counted verdict.", Consensus: m.checkConsensus}, m.usage, nil
}

func (m *countingModerator) calls() (checks, summaries, verdicts int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.checks, m.summaries, m.verdicts
}

type failingMessageStorage struct {
	core.Storage
	failKind  string
	attempted chan struct{}
	once      sync.Once
}

type failingTransitionStorage struct {
	core.Storage
	status    core.DebateStatus
	round     int
	attempted chan struct{}
	once      sync.Once
}

func (s *failingTransitionStorage) UpdateDebate(debate core.Debate) error {
	if debate.Status == s.status && debate.CurrentRound == s.round {
		failed := false
		s.once.Do(func() {
			failed = true
			close(s.attempted)
		})
		if failed {
			return errors.New("injected state-transition write failure")
		}
	}
	return s.Storage.UpdateDebate(debate)
}

type failingTranscriptReadStorage struct {
	core.Storage
	mu        sync.Mutex
	reads     int
	failRead  int
	attempted chan struct{}
	once      sync.Once
}

func (s *failingTranscriptReadStorage) Messages(debateID string, afterSeq int64) ([]core.Message, error) {
	s.mu.Lock()
	s.reads++
	read := s.reads
	s.mu.Unlock()
	if read == s.failRead {
		s.once.Do(func() { close(s.attempted) })
		return nil, errors.New("injected transcript read failure")
	}
	return s.Storage.Messages(debateID, afterSeq)
}

func (s *failingMessageStorage) AddMessage(message core.Message) (int64, error) {
	if message.Kind == s.failKind {
		s.once.Do(func() { close(s.attempted) })
		return 0, errors.New("injected structured-message write failure")
	}
	return s.Storage.AddMessage(message)
}

func (m *blockingConsensusModerator) CheckRound(
	ctx context.Context,
	question string,
	transcript string,
	round int,
	allowedSeqs []int64,
) (core.RoundSummary, core.ModerationUsage, error) {
	m.startedOnce.Do(func() { close(m.started) })
	select {
	case <-m.release:
		return m.consensusModerator.CheckRound(ctx, question, transcript, round, allowedSeqs)
	case <-ctx.Done():
		return core.RoundSummary{}, core.ModerationUsage{}, ctx.Err()
	}
}

func (m *blockingConsensusModerator) unblock() {
	m.releaseOnce.Do(func() { close(m.release) })
}

func (unresolvedConsensusModerator) CheckRound(
	context.Context,
	string,
	string,
	int,
	[]int64,
) (core.RoundSummary, core.ModerationUsage, error) {
	return core.RoundSummary{
		Summary:             "One question remains open.",
		UnresolvedQuestions: []string{"Which option should be chosen?"},
		Consensus:           true,
	}, core.ModerationUsage{}, nil
}

func (consensusSummaryModerator) Summary(
	context.Context,
	string,
	string,
	int,
	[]int64,
) (core.RoundSummary, core.ModerationUsage, error) {
	return core.RoundSummary{Summary: "Provider incorrectly claims consensus.", Consensus: true},
		core.ModerationUsage{}, nil
}

func (unavailableVerdictModerator) Verdict(
	context.Context,
	string,
	string,
	[]int64,
) (core.ModerationVerdict, core.ModerationUsage, error) {
	return core.ModerationVerdict{}, core.ModerationUsage{}, errors.New("moderator unavailable")
}

func (consensusModerator) Name() string { return "test moderator" }

func (consensusModerator) CheckRound(
	context.Context,
	string,
	string,
	int,
	[]int64,
) (core.RoundSummary, core.ModerationUsage, error) {
	return core.RoundSummary{
		Summary:   "The tested invariants hold.",
		Decisions: []string{"Conclude the debate."},
		Consensus: true,
	}, core.ModerationUsage{}, nil
}

func (consensusModerator) Summary(
	context.Context,
	string,
	string,
	int,
	[]int64,
) (core.RoundSummary, core.ModerationUsage, error) {
	return core.RoundSummary{}, core.ModerationUsage{}, errors.New("unexpected hybrid summary")
}

func (consensusModerator) Verdict(
	context.Context,
	string,
	string,
	[]int64,
) (core.ModerationVerdict, core.ModerationUsage, error) {
	return core.ModerationVerdict{
		FinalAnswer: "Consensus reached.",
		Decisions:   []string{"Keep the agreed position."},
		Consensus:   true,
	}, core.ModerationUsage{}, nil
}

func TestDebateStateMachineReachesConsensus(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	moderator := &blockingConsensusModerator{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	t.Cleanup(moderator.unblock)
	service := newTestServiceWithOptions(t, moderator, core.WithClock(func() time.Time { return now }))
	creator := registerAgent(t, service, "creator")
	challenger := registerAgent(t, service, "challenger")

	created, err := service.CreateDebate(creator, core.CreateDebateParams{
		Question:       "Which approach should we use?",
		Description:    "Visible only after the debate starts.",
		Mode:           core.ModeModerator,
		Rounds:         3,
		TurnTimeoutSec: core.MinTurnTimeout,
		PrepTimeSec:    1,
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

	// Participant ordering is chronological; make it explicit under the fake clock.
	now = now.Add(time.Second)
	if _, err := service.JoinDebate(challenger, created.ID, "challenge the proposal"); err != nil {
		t.Fatalf("JoinDebate: %v", err)
	}
	started, err := service.StartDebate(creator, created.ID)
	if err != nil {
		t.Fatalf("StartDebate: %v", err)
	}
	if started.Status != core.StatusPreparing || started.CurrentRound != 0 {
		t.Fatalf("started state = %s round %d", started.Status, started.CurrentRound)
	}
	if started.TurnAgentID != "" {
		t.Fatalf("preparing turn = %q, want none", started.TurnAgentID)
	}
	if started.Description == "" {
		t.Fatal("description must be visible after the debate starts")
	}

	now = now.Add(2 * time.Second)
	runCtx, cancelRun := context.WithCancel(context.Background())
	t.Cleanup(cancelRun)
	go service.Run(runCtx)
	running := waitForStatusWithin(t, service, created.ID, core.StatusRunning, time.Second)
	if running.CurrentRound != 1 || running.TurnAgentID != creator.ID {
		t.Fatalf("first running state = round:%d turn:%q, want round 1 turn %q",
			running.CurrentRound, running.TurnAgentID, creator.ID)
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
	select {
	case <-moderator.started:
	case <-time.After(2 * time.Second):
		t.Fatal("moderator did not start")
	}
	moderating, err := service.GetDebate(created.ID)
	if err != nil {
		t.Fatalf("GetDebate while moderating: %v", err)
	}
	if moderating.Status != core.StatusModerating || moderating.CurrentRound != 1 {
		t.Fatalf("moderating state = %s round %d", moderating.Status, moderating.CurrentRound)
	}
	moderator.unblock()

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
	if messages[2].RoundSummary == nil || messages[2].RoundSummary.Summary != "The tested invariants hold." {
		t.Fatalf("structured round summary was not retained: %+v", messages[2].RoundSummary)
	}
	if messages[3].Verdict == nil || messages[3].Verdict.FinalAnswer != "Consensus reached." {
		t.Fatalf("structured verdict was not retained: %+v", messages[3].Verdict)
	}
}

func TestRestartExpiresTurnMissedWhileServiceWasStopped(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	database, err := store.Open(filepath.Join(t.TempDir(), "court.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := core.NewService(database, core.NewHub(), consensusModerator{}, logger,
		core.WithClock(func() time.Time { return now }))
	creator := registerAgent(t, service, "creator")
	challenger := registerAgent(t, service, "challenger")
	created, err := service.CreateDebate(creator, core.CreateDebateParams{
		Question: "Who moves after an offline deadline?", Rounds: 2,
		TurnTimeoutSec: core.MinTurnTimeout,
	})
	if err != nil {
		t.Fatalf("CreateDebate: %v", err)
	}
	if _, err := service.JoinDebate(challenger, created.ID, "challenge"); err != nil {
		t.Fatalf("JoinDebate: %v", err)
	}
	started, err := service.StartDebate(creator, created.ID)
	if err != nil {
		t.Fatalf("StartDebate: %v", err)
	}
	expiredAgentID, expiredAgentName := started.TurnAgentID, creator.Name
	nextAgentID := challenger.ID
	if expiredAgentID == challenger.ID {
		expiredAgentName = challenger.Name
		nextAgentID = creator.ID
	}

	// No background service is running while the first turn expires.
	now = now.Add(time.Duration(core.MinTurnTimeout+1) * time.Second)
	restartHub := core.NewHub()
	restarted := core.NewService(database, restartHub, consensusModerator{}, logger,
		core.WithClock(func() time.Time { return now }))
	events := restarted.Subscribe(created.ID)
	defer restarted.Unsubscribe(created.ID, events)
	runCtx, cancelRun := context.WithCancel(context.Background())
	t.Cleanup(cancelRun)
	go restarted.Run(runCtx)

	afterRestart := waitForTurn(t, restarted, created.ID, nextAgentID)
	if afterRestart.CurrentRound != 1 {
		t.Fatalf("round after skipped turn = %d, want 1", afterRestart.CurrentRound)
	}
	messages, err := restarted.Messages(created.ID, 0)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(messages) != 1 || messages[0].Kind != core.KindSystem ||
		!strings.Contains(messages[0].Text, expiredAgentName+" пропустил ход") {
		t.Fatalf("messages after restart = %+v, want one skipped-turn system message", messages)
	}

	wantEvents := map[string]bool{core.EventSkipped: false, core.EventTurn: false}
	deadline := time.After(2 * time.Second)
	for !wantEvents[core.EventSkipped] || !wantEvents[core.EventTurn] {
		select {
		case event := <-events:
			if _, tracked := wantEvents[event.Type]; tracked {
				wantEvents[event.Type] = true
			}
		case <-deadline:
			t.Fatalf("restart events = %+v, want skipped and turn", wantEvents)
		}
	}
}

func TestHybridUnanimityConcludesBeforeFinalRound(t *testing.T) {
	service := newTestServiceWithModerator(t, unavailableVerdictModerator{})
	creator := registerAgent(t, service, "creator")
	challenger := registerAgent(t, service, "challenger")
	created, err := service.CreateDebate(creator, core.CreateDebateParams{
		Question: "Can unanimous votes conclude early?", Mode: core.ModeHybrid,
		Rounds: 3, TurnTimeoutSec: core.MinTurnTimeout,
	})
	if err != nil {
		t.Fatalf("CreateDebate: %v", err)
	}
	if _, err := service.JoinDebate(challenger, created.ID, "preferred option"); err != nil {
		t.Fatalf("JoinDebate: %v", err)
	}
	if _, err := service.StartDebate(creator, created.ID); err != nil {
		t.Fatalf("StartDebate: %v", err)
	}
	if _, err := service.PostArgument(context.Background(), creator, created.ID,
		"I support the challenger's option.", challenger.ID); err != nil {
		t.Fatalf("creator PostArgument: %v", err)
	}
	if _, err := service.PostArgument(context.Background(), challenger, created.ID,
		"I maintain my option.", ""); err != nil {
		t.Fatalf("challenger PostArgument: %v", err)
	}

	concluded := waitForStatus(t, service, created.ID, core.StatusConcluded)
	if !concluded.Consensus || concluded.CurrentRound != 1 {
		t.Fatalf("hybrid conclusion = consensus:%t round:%d, want true in round 1",
			concluded.Consensus, concluded.CurrentRound)
	}
	messages, err := service.Messages(created.ID, 0)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	verdict := messages[len(messages)-1].Verdict
	if verdict == nil || !verdict.Consensus {
		t.Fatalf("hybrid verdict = %+v, want consensus", verdict)
	}
}

func TestModeratorCannotConcludeWhileQuestionsRemainOpen(t *testing.T) {
	service := newTestServiceWithModerator(t, unresolvedConsensusModerator{})
	creator := registerAgent(t, service, "creator")
	challenger := registerAgent(t, service, "challenger")
	created, err := service.CreateDebate(creator, core.CreateDebateParams{
		Question: "Is the decision complete?", Mode: core.ModeModerator,
		Rounds: 2, TurnTimeoutSec: core.MinTurnTimeout,
	})
	if err != nil {
		t.Fatalf("CreateDebate: %v", err)
	}
	if _, err := service.JoinDebate(challenger, created.ID, "not yet"); err != nil {
		t.Fatalf("JoinDebate: %v", err)
	}
	if _, err := service.StartDebate(creator, created.ID); err != nil {
		t.Fatalf("StartDebate: %v", err)
	}
	if _, err := service.PostArgument(context.Background(), creator, created.ID, "option A", ""); err != nil {
		t.Fatalf("creator PostArgument: %v", err)
	}
	if _, err := service.PostArgument(context.Background(), challenger, created.ID, "question A", ""); err != nil {
		t.Fatalf("challenger PostArgument: %v", err)
	}

	roundTwo := waitForRound(t, service, created.ID, core.StatusRunning, 2)
	if roundTwo.Consensus {
		t.Fatal("debate reported consensus while a question remained open")
	}
	messages, err := service.Messages(created.ID, 0)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(messages) != 3 || messages[2].RoundSummary == nil ||
		messages[2].RoundSummary.Consensus || len(messages[2].RoundSummary.UnresolvedQuestions) != 1 {
		t.Fatalf("stored summary = %+v, want non-consensus with one open question", messages[2])
	}
}

func TestRecoveryRejectsStoredConsensusWithOpenQuestions(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	database, err := store.Open(filepath.Join(t.TempDir(), "court.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := core.NewService(database, core.NewHub(), consensusModerator{}, logger,
		core.WithClock(func() time.Time { return now }))
	creator := registerAgent(t, service, "creator")
	challenger := registerAgent(t, service, "challenger")
	created, err := service.CreateDebate(creator, core.CreateDebateParams{
		Question: "Can recovery trust contradictory evidence?", Rounds: 2,
		TurnTimeoutSec: core.MinTurnTimeout,
	})
	if err != nil {
		t.Fatalf("CreateDebate: %v", err)
	}
	if _, err := service.JoinDebate(challenger, created.ID, "challenge"); err != nil {
		t.Fatalf("JoinDebate: %v", err)
	}
	if _, err := service.StartDebate(creator, created.ID); err != nil {
		t.Fatalf("StartDebate: %v", err)
	}

	stored, err := database.GetDebate(created.ID)
	if err != nil {
		t.Fatalf("GetDebate: %v", err)
	}
	stored.Status = core.StatusModerating
	stored.TurnAgentID = ""
	stored.TurnDeadline = time.Time{}
	if err := database.UpdateDebate(stored); err != nil {
		t.Fatalf("UpdateDebate: %v", err)
	}
	contradictory := core.RoundSummary{
		Summary:             "An earlier binary claimed consensus.",
		UnresolvedQuestions: []string{"Which option should be chosen?"},
		Consensus:           true,
	}
	if _, err := database.AddMessage(core.Message{
		DebateID: created.ID, Round: 1, SpeakerName: "legacy moderator",
		Kind: core.KindSummary, Text: contradictory.Text(), RoundSummary: &contradictory,
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	moderator := &countingModerator{}
	restarted := core.NewService(database, core.NewHub(), moderator, logger,
		core.WithClock(func() time.Time { return now }))
	runCtx, cancelRun := context.WithCancel(context.Background())
	t.Cleanup(cancelRun)
	go restarted.Run(runCtx)

	roundTwo := waitForRound(t, restarted, created.ID, core.StatusRunning, 2)
	if roundTwo.Consensus {
		t.Fatal("recovered debate reported consensus from contradictory stored evidence")
	}
	checks, summaries, verdicts := moderator.calls()
	if checks != 0 || summaries != 0 || verdicts != 0 {
		t.Fatalf("moderator calls during recovery = %d/%d/%d, want 0/0/0",
			checks, summaries, verdicts)
	}
}

func TestAuthenticateRejectsUnknownKey(t *testing.T) {
	service := newTestService(t)
	if _, err := service.Authenticate("unknown-key"); !errors.Is(err, core.ErrUnauthorized) {
		t.Fatalf("Authenticate error = %v, want ErrUnauthorized", err)
	}
}

func TestAuthenticateAcceptsRegisteredCredential(t *testing.T) {
	service := newTestService(t)
	agent, key, err := service.RegisterAgent("registered", "test participant")
	if err != nil {
		t.Fatalf("RegisterAgent: %v", err)
	}
	authenticated, err := service.Authenticate(key)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if authenticated.ID != agent.ID {
		t.Fatalf("authenticated agent = %q, want %q", authenticated.ID, agent.ID)
	}
}

func TestTurnStatusUsesInjectedClock(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	service := newTestServiceWithOptions(t, consensusModerator{}, core.WithClock(func() time.Time { return now }))
	creator := registerAgent(t, service, "creator")
	challenger := registerAgent(t, service, "challenger")
	created, err := service.CreateDebate(creator, core.CreateDebateParams{
		Question: "Does the deadline follow the injected clock?", TurnTimeoutSec: core.MinTurnTimeout,
	})
	if err != nil {
		t.Fatalf("CreateDebate: %v", err)
	}
	if _, err := service.JoinDebate(challenger, created.ID, "verify"); err != nil {
		t.Fatalf("JoinDebate: %v", err)
	}
	if _, err := service.StartDebate(creator, created.ID); err != nil {
		t.Fatalf("StartDebate: %v", err)
	}

	status, err := service.TurnStatus(creator, created.ID)
	if err != nil {
		t.Fatalf("TurnStatus: %v", err)
	}
	if status.DeadlineSec != core.MinTurnTimeout {
		t.Fatalf("initial deadline = %d, want %d", status.DeadlineSec, core.MinTurnTimeout)
	}
	now = now.Add(7 * time.Second)
	status, err = service.TurnStatus(creator, created.ID)
	if err != nil {
		t.Fatalf("TurnStatus after clock advance: %v", err)
	}
	if status.DeadlineSec != core.MinTurnTimeout-7 {
		t.Fatalf("advanced deadline = %d, want %d", status.DeadlineSec, core.MinTurnTimeout-7)
	}
}

func TestHybridVerdictConsensusFollowsVotesNotModerator(t *testing.T) {
	service := newTestService(t)
	creator := registerAgent(t, service, "creator")
	challenger := registerAgent(t, service, "challenger")
	created, err := service.CreateDebate(creator, core.CreateDebateParams{
		Question:       "Do the votes agree?",
		Mode:           core.ModeHybrid,
		Rounds:         1,
		TurnTimeoutSec: core.MinTurnTimeout,
	})
	if err != nil {
		t.Fatalf("CreateDebate: %v", err)
	}
	if _, err := service.JoinDebate(challenger, created.ID, "no"); err != nil {
		t.Fatalf("JoinDebate: %v", err)
	}
	if _, err := service.StartDebate(creator, created.ID); err != nil {
		t.Fatalf("StartDebate: %v", err)
	}
	// Each participant implicitly supports itself, so votes are split. The
	// test moderator returns Consensus=true; hybrid semantics must override it.
	if _, err := service.PostArgument(context.Background(), creator, created.ID, "yes", ""); err != nil {
		t.Fatalf("creator PostArgument: %v", err)
	}
	if _, err := service.PostArgument(context.Background(), challenger, created.ID, "no", ""); err != nil {
		t.Fatalf("challenger PostArgument: %v", err)
	}
	concluded := waitForStatus(t, service, created.ID, core.StatusConcluded)
	if concluded.Consensus {
		t.Fatal("hybrid debate reported consensus despite split votes")
	}
	messages, err := service.Messages(created.ID, 0)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	verdict := messages[len(messages)-1].Verdict
	if verdict == nil || verdict.Consensus {
		t.Fatalf("stored hybrid verdict consensus = %+v, want false", verdict)
	}
}

func TestHybridIntermediateSummaryCannotClaimConsensus(t *testing.T) {
	service := newTestServiceWithModerator(t, consensusSummaryModerator{})
	creator := registerAgent(t, service, "creator")
	challenger := registerAgent(t, service, "challenger")
	created, err := service.CreateDebate(creator, core.CreateDebateParams{
		Question:       "Continue after a split vote?",
		Mode:           core.ModeHybrid,
		Rounds:         2,
		TurnTimeoutSec: core.MinTurnTimeout,
	})
	if err != nil {
		t.Fatalf("CreateDebate: %v", err)
	}
	if _, err := service.JoinDebate(challenger, created.ID, "no"); err != nil {
		t.Fatalf("JoinDebate: %v", err)
	}
	if _, err := service.StartDebate(creator, created.ID); err != nil {
		t.Fatalf("StartDebate: %v", err)
	}
	if _, err := service.PostArgument(context.Background(), creator, created.ID, "yes", ""); err != nil {
		t.Fatalf("creator PostArgument: %v", err)
	}
	if _, err := service.PostArgument(context.Background(), challenger, created.ID, "no", ""); err != nil {
		t.Fatalf("challenger PostArgument: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		debate, err := service.GetDebate(created.ID)
		if err != nil {
			t.Fatalf("GetDebate: %v", err)
		}
		if debate.Status == core.StatusRunning && debate.CurrentRound == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("debate did not advance to round 2: status=%s round=%d", debate.Status, debate.CurrentRound)
		}
		time.Sleep(10 * time.Millisecond)
	}
	messages, err := service.Messages(created.ID, 0)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	var summary *core.RoundSummary
	for _, message := range messages {
		if message.Kind == core.KindSummary {
			summary = message.RoundSummary
		}
	}
	if summary == nil || summary.Consensus {
		t.Fatalf("stored hybrid round summary = %+v, want consensus=false", summary)
	}
}

func TestStructuredModerationWriteFailurePreventsStateTransition(t *testing.T) {
	for _, tt := range []struct {
		name     string
		failKind string
		rounds   int
	}{
		{name: "summary", failKind: core.KindSummary, rounds: 2},
		{name: "verdict", failKind: core.KindVerdict, rounds: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			database, err := store.Open(filepath.Join(t.TempDir(), "court.db"))
			if err != nil {
				t.Fatalf("store.Open: %v", err)
			}
			t.Cleanup(func() { _ = database.Close() })
			failing := &failingMessageStorage{
				Storage: database, failKind: tt.failKind, attempted: make(chan struct{}),
			}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			service := core.NewService(failing, core.NewHub(), consensusModerator{}, logger)
			creator := registerAgent(t, service, "creator")
			challenger := registerAgent(t, service, "challenger")
			created, err := service.CreateDebate(creator, core.CreateDebateParams{
				Question: "Must evidence be durable?", Mode: core.ModeModerator,
				Rounds: tt.rounds, TurnTimeoutSec: core.MinTurnTimeout,
			})
			if err != nil {
				t.Fatalf("CreateDebate: %v", err)
			}
			if _, err := service.JoinDebate(challenger, created.ID, "challenge"); err != nil {
				t.Fatalf("JoinDebate: %v", err)
			}
			if _, err := service.StartDebate(creator, created.ID); err != nil {
				t.Fatalf("StartDebate: %v", err)
			}
			events := service.Subscribe(created.ID)
			defer service.Unsubscribe(created.ID, events)
			if _, err := service.PostArgument(context.Background(), creator, created.ID, "first", ""); err != nil {
				t.Fatalf("creator PostArgument: %v", err)
			}
			if _, err := service.PostArgument(context.Background(), challenger, created.ID, "second", ""); err != nil {
				t.Fatalf("challenger PostArgument: %v", err)
			}
			select {
			case <-failing.attempted:
			case <-time.After(2 * time.Second):
				t.Fatalf("moderation did not attempt to persist %s", tt.failKind)
			}
			time.Sleep(20 * time.Millisecond)
			debate, err := service.GetDebate(created.ID)
			if err != nil {
				t.Fatalf("GetDebate: %v", err)
			}
			if debate.Status != core.StatusModerating || debate.CurrentRound != 1 {
				t.Fatalf("debate advanced after failed %s write: status=%s round=%d", tt.failKind, debate.Status, debate.CurrentRound)
			}
			messages, err := service.Messages(created.ID, 0)
			if err != nil {
				t.Fatalf("Messages: %v", err)
			}
			for _, message := range messages {
				if message.Kind == tt.failKind {
					t.Fatalf("failed %s unexpectedly became durable: %+v", tt.failKind, message)
				}
			}
			for {
				select {
				case event := <-events:
					if event.Type == core.EventConcluded || (event.Type == core.EventTurn && event.Round > 1) {
						t.Fatalf("published transition %s after failed %s write", event.Type, tt.failKind)
					}
				default:
					return
				}
			}
		})
	}
}

func TestRecoveryDoesNotDuplicatePersistedModerationAfterTransitionFailure(t *testing.T) {
	for _, tt := range []struct {
		name           string
		mode           core.DebateMode
		failKind       string
		rounds         int
		status         core.DebateStatus
		round          int
		checkConsensus bool
		verdictErr     bool
		wantChecks     int
		wantSummaries  int
		wantVerdicts   int
		eventType      string
	}{
		{name: "moderator summary before next round", mode: core.ModeModerator,
			failKind: core.KindSummary, rounds: 2, status: core.StatusRunning, round: 2,
			wantChecks: 1, eventType: core.EventTurn},
		{name: "moderator early-consensus verdict before conclusion", mode: core.ModeModerator,
			failKind: core.KindVerdict, rounds: 2, status: core.StatusConcluded, round: 1,
			checkConsensus: true, wantChecks: 1, wantVerdicts: 1, eventType: core.EventConcluded},
		{name: "hybrid summary before next round", mode: core.ModeHybrid,
			failKind: core.KindSummary, rounds: 2, status: core.StatusRunning, round: 2,
			wantSummaries: 1, eventType: core.EventTurn},
		{name: "hybrid fallback verdict before conclusion", mode: core.ModeHybrid,
			failKind: core.KindVerdict, rounds: 1, status: core.StatusConcluded, round: 1,
			verdictErr: true, wantVerdicts: 1, eventType: core.EventConcluded},
	} {
		t.Run(tt.name, func(t *testing.T) {
			database, err := store.Open(filepath.Join(t.TempDir(), "court.db"))
			if err != nil {
				t.Fatalf("store.Open: %v", err)
			}
			t.Cleanup(func() { _ = database.Close() })
			failing := &failingTransitionStorage{
				Storage: database, status: tt.status, round: tt.round, attempted: make(chan struct{}),
			}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			moderator := &countingModerator{checkConsensus: tt.checkConsensus, verdictErr: tt.verdictErr}
			service := core.NewService(failing, core.NewHub(), moderator, logger)
			creator := registerAgent(t, service, "creator")
			challenger := registerAgent(t, service, "challenger")
			created, err := service.CreateDebate(creator, core.CreateDebateParams{
				Question: "Recover exactly once?", Mode: tt.mode,
				Rounds: tt.rounds, TurnTimeoutSec: core.MinTurnTimeout,
			})
			if err != nil {
				t.Fatalf("CreateDebate: %v", err)
			}
			if _, err := service.JoinDebate(challenger, created.ID, "challenge"); err != nil {
				t.Fatalf("JoinDebate: %v", err)
			}
			if _, err := service.StartDebate(creator, created.ID); err != nil {
				t.Fatalf("StartDebate: %v", err)
			}
			if _, err := service.PostArgument(context.Background(), creator, created.ID, "first", ""); err != nil {
				t.Fatalf("creator PostArgument: %v", err)
			}
			if _, err := service.PostArgument(context.Background(), challenger, created.ID, "second", ""); err != nil {
				t.Fatalf("challenger PostArgument: %v", err)
			}
			select {
			case <-failing.attempted:
			case <-time.After(2 * time.Second):
				t.Fatal("moderation did not reach the injected transition failure")
			}
			stuck, err := database.GetDebate(created.ID)
			if err != nil || stuck.Status != core.StatusModerating {
				t.Fatalf("durable state after transition failure = %+v, err=%v", stuck, err)
			}

			recoveryHub := core.NewHub()
			events := recoveryHub.Subscribe(created.ID)
			defer recoveryHub.Unsubscribe(created.ID, events)
			recovered := core.NewService(failing, recoveryHub, moderator, logger)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go recovered.Run(ctx)
			deadline := time.Now().Add(2 * time.Second)
			for {
				debate, err := recovered.GetDebate(created.ID)
				if err != nil {
					t.Fatalf("GetDebate during recovery: %v", err)
				}
				if debate.Status == tt.status && debate.CurrentRound == tt.round {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("recovery did not reach status=%s round=%d: got status=%s round=%d",
						tt.status, tt.round, debate.Status, debate.CurrentRound)
				}
				time.Sleep(10 * time.Millisecond)
			}
			messages, err := database.Messages(created.ID, 0)
			if err != nil {
				t.Fatalf("Messages after recovery: %v", err)
			}
			count := 0
			for _, message := range messages {
				if message.Kind == tt.failKind {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("%s count after recovery = %d, want exactly 1", tt.failKind, count)
			}
			checks, summaries, verdicts := moderator.calls()
			if checks != tt.wantChecks || summaries != tt.wantSummaries || verdicts != tt.wantVerdicts {
				t.Fatalf("moderator calls after recovery = check:%d summary:%d verdict:%d, want %d/%d/%d",
					checks, summaries, verdicts, tt.wantChecks, tt.wantSummaries, tt.wantVerdicts)
			}
			select {
			case event := <-events:
				if event.Type == core.EventMessage {
					t.Fatalf("recovery republished durable moderation as a new message: %+v", event)
				}
				if event.Type != tt.eventType {
					t.Fatalf("recovery event = %s, want %s", event.Type, tt.eventType)
				}
			case <-time.After(time.Second):
				t.Fatalf("recovery did not publish %s", tt.eventType)
			}
		})
	}
}

func TestTranscriptRereadFailureBlocksVerdictAndConclusion(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "court.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	failing := &failingTranscriptReadStorage{
		Storage: database, failRead: 2, attempted: make(chan struct{}),
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := core.NewService(failing, core.NewHub(), consensusModerator{}, logger)
	creator := registerAgent(t, service, "creator")
	challenger := registerAgent(t, service, "challenger")
	created, err := service.CreateDebate(creator, core.CreateDebateParams{
		Question: "Require complete transcript?", Mode: core.ModeModerator,
		Rounds: 2, TurnTimeoutSec: core.MinTurnTimeout,
	})
	if err != nil {
		t.Fatalf("CreateDebate: %v", err)
	}
	if _, err := service.JoinDebate(challenger, created.ID, "challenge"); err != nil {
		t.Fatalf("JoinDebate: %v", err)
	}
	if _, err := service.StartDebate(creator, created.ID); err != nil {
		t.Fatalf("StartDebate: %v", err)
	}
	if _, err := service.PostArgument(context.Background(), creator, created.ID, "first", ""); err != nil {
		t.Fatalf("creator PostArgument: %v", err)
	}
	if _, err := service.PostArgument(context.Background(), challenger, created.ID, "second", ""); err != nil {
		t.Fatalf("challenger PostArgument: %v", err)
	}
	select {
	case <-failing.attempted:
	case <-time.After(2 * time.Second):
		t.Fatal("moderation did not reach the injected transcript reread failure")
	}
	time.Sleep(20 * time.Millisecond)
	debate, err := database.GetDebate(created.ID)
	if err != nil || debate.Status != core.StatusModerating {
		t.Fatalf("state after transcript reread failure = %+v, err=%v", debate, err)
	}
	messages, err := database.Messages(created.ID, 0)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	summaries, verdicts := 0, 0
	for _, message := range messages {
		switch message.Kind {
		case core.KindSummary:
			summaries++
		case core.KindVerdict:
			verdicts++
		}
	}
	if summaries != 1 || verdicts != 0 {
		t.Fatalf("moderation records after incomplete reread: summaries=%d verdicts=%d", summaries, verdicts)
	}
}

func TestHybridFallbackRetainsLegacyTextAndStructuredVerdict(t *testing.T) {
	service := newTestServiceWithModerator(t, unavailableVerdictModerator{})
	creator := registerAgent(t, service, "creator")
	challenger := registerAgent(t, service, "challenger")
	created, err := service.CreateDebate(creator, core.CreateDebateParams{
		Question:       "Fallback?",
		Mode:           core.ModeHybrid,
		Rounds:         1,
		TurnTimeoutSec: core.MinTurnTimeout,
	})
	if err != nil {
		t.Fatalf("CreateDebate: %v", err)
	}
	if _, err := service.JoinDebate(challenger, created.ID, "no"); err != nil {
		t.Fatalf("JoinDebate: %v", err)
	}
	if _, err := service.StartDebate(creator, created.ID); err != nil {
		t.Fatalf("StartDebate: %v", err)
	}
	if _, err := service.PostArgument(context.Background(), creator, created.ID, "yes", ""); err != nil {
		t.Fatalf("creator PostArgument: %v", err)
	}
	if _, err := service.PostArgument(context.Background(), challenger, created.ID, "no", ""); err != nil {
		t.Fatalf("challenger PostArgument: %v", err)
	}
	waitForStatus(t, service, created.ID, core.StatusConcluded)
	messages, err := service.Messages(created.ID, 0)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	message := messages[len(messages)-1]
	wantText := "Консенсус не достигнут. Дебаты завершены по исчерпанию раундов.\n\n" +
		"Голоса участников:\n" +
		"- creator → creator\n" +
		"- challenger → challenger\n" +
		"\nГолоса разделились поровну — итоговая позиция не определена.\n"
	if message.Text != wantText {
		t.Fatalf("fallback text = %q, want exact pre-v1 text %q", message.Text, wantText)
	}
	if message.Verdict == nil || message.Verdict.FinalAnswer != strings.TrimSpace(wantText) || message.Verdict.Consensus {
		t.Fatalf("structured fallback verdict = %+v", message.Verdict)
	}
}

func newTestService(t *testing.T) *core.Service {
	return newTestServiceWithModerator(t, consensusModerator{})
}

func newTestServiceWithModerator(t *testing.T, mod core.Moderator) *core.Service {
	return newTestServiceWithOptions(t, mod)
}

func newTestServiceWithOptions(t *testing.T, mod core.Moderator, options ...core.ServiceOption) *core.Service {
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
	return core.NewService(database, core.NewHub(), mod, logger, options...)
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
	return waitForStatusWithin(t, service, debateID, want, 2*time.Second)
}

func waitForStatusWithin(
	t *testing.T,
	service *core.Service,
	debateID string,
	want core.DebateStatus,
	timeout time.Duration,
) core.DebateView {
	t.Helper()
	deadline := time.Now().Add(timeout)
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

func waitForRound(
	t *testing.T,
	service *core.Service,
	debateID string,
	wantStatus core.DebateStatus,
	wantRound int,
) core.DebateView {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		debate, err := service.GetDebate(debateID)
		if err != nil {
			t.Fatalf("GetDebate: %v", err)
		}
		if debate.Status == wantStatus && debate.CurrentRound == wantRound {
			return debate
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("debate did not reach status %q round %d", wantStatus, wantRound)
	return core.DebateView{}
}

func waitForTurn(t *testing.T, service *core.Service, debateID, wantAgentID string) core.DebateView {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		debate, err := service.GetDebate(debateID)
		if err != nil {
			t.Fatalf("GetDebate: %v", err)
		}
		if debate.Status == core.StatusRunning && debate.TurnAgentID == wantAgentID {
			return debate
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("debate did not assign turn to %q", wantAgentID)
	return core.DebateView{}
}
