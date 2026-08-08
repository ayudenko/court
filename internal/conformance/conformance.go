// Package conformance checks a debate artifact against the normative rules of
// SPEC.md. It reads only the exported v1 JSONL stream, never service internals,
// so it judges any implementation of the protocol by the same evidence: the
// artifact that implementation publishes.
//
// Every rule here has an identifier of the form C<n> that SPEC.md cites, and
// every rule SPEC.md states as MUST is checked here. A statement SPEC.md makes
// without a rule identifier is descriptive and is deliberately not enforced —
// see docs/adr/0007-protocol-conformance-suite.md for why that separation is
// the point rather than an omission.
package conformance

import (
	"fmt"
	"sort"

	"court/internal/core"
	"court/internal/protocol"
)

// Violation is one normative rule broken by an artifact.
type Violation struct {
	// Rule is the SPEC.md rule identifier, for example "C4".
	Rule string
	// Detail names the specific record and the way it broke the rule.
	Detail string
}

func (v Violation) String() string { return v.Rule + ": " + v.Detail }

// Check decodes a canonical v1 JSONL artifact and reports every normative rule
// it breaks. A nil result means the artifact conforms.
//
// A decode failure is reported as the single violation C0 rather than an error:
// a conformance report answers "which rules does this artifact break", and an
// unreadable artifact breaks the first one.
func Check(data []byte) []Violation {
	// DecodeRecords, not DecodeJSONL: the latter repairs record order on read,
	// and an artifact's order is one of the things C0 judges. A reader that
	// silently sorts cannot report that the producer did not.
	records, err := protocol.DecodeRecords(data)
	if err != nil {
		return []Violation{{Rule: "C0", Detail: err.Error()}}
	}
	return CheckRecords(records)
}

// CheckRecords applies every rule, including C0, to already decoded records.
//
// C0 is enforced here rather than assumed. A caller that decoded the stream with
// its own reader is exactly the caller this package is for, and answering it
// with a panic — or with a clean report on a stream Check would have rejected —
// would make the suite useless for judging an artifact from elsewhere.
func CheckRecords(records []protocol.ExportRecord) []Violation {
	for index, record := range records {
		if err := record.Validate(); err != nil {
			return []Violation{{Rule: "C0", Detail: fmt.Sprintf("record %d: %v", index+1, err)}}
		}
	}
	// Пер-запись Validate не видит потока: вторую debate-запись и дубли ключей
	// ловит только эта проверка, и без неё CheckRecords принимал бы то, что
	// Check на тех же байтах отвергает.
	canonical, err := protocol.Canonicalize(records)
	if err != nil {
		return []Violation{{Rule: "C0", Detail: err.Error()}}
	}
	// Порядок — часть контракта, а не деталь чтения: потребитель, читающий
	// поток на лету, полагается на то, что голоса идут после протокола, а
	// участники до него. Записи проверяются дальше в каноническом порядке, но
	// producer, приславший другой, обязан узнать об этом.
	orderViolations := checkCanonicalOrder(records, canonical)
	artifact, violations := split(canonical)
	violations = append(orderViolations, violations...)
	if artifact == nil {
		return violations
	}
	for _, rule := range rules {
		if rule.check == nil {
			continue // C0 is decided by Check before the records exist
		}
		violations = append(violations, rule.check(artifact)...)
	}
	return violations
}

func checkCanonicalOrder(presented, canonical []protocol.ExportRecord) []Violation {
	if len(presented) != len(canonical) {
		// Недостижимо: Canonicalize либо возвращает ошибку, либо ровно те же
		// записи. Возврат nil здесь означал бы, что проверка порядка молча
		// исчезла, поэтому расхождение длин — это находка, а не пропуск.
		return []Violation{{Rule: "C0", Detail: fmt.Sprintf(
			"canonical form has %d records, the artifact presented %d", len(canonical), len(presented))}}
	}
	for index := range presented {
		got, want := streamKey(presented[index]), streamKey(canonical[index])
		if got != want {
			return []Violation{{Rule: "C0", Detail: fmt.Sprintf(
				"record %d is %s; canonical order puts %s there", index+1, got, want)}}
		}
	}
	return nil
}

// streamKey names a record by what fixes its canonical position.
func streamKey(record protocol.ExportRecord) string {
	switch record.RecordType {
	case protocol.RecordDebate:
		return "the debate record"
	case protocol.RecordParticipant:
		return fmt.Sprintf("participant %s", record.Participant.AgentID)
	case protocol.RecordVote:
		return fmt.Sprintf("the vote of %s", record.Vote.AgentID)
	default:
		return fmt.Sprintf("%s at seq %d", record.RecordType, transcriptSeq(record))
	}
}

// Rule is one normative rule of SPEC.md together with the check that enforces
// it. The registry is exported so a test can require that SPEC.md documents
// exactly these identifiers and that each one has a rejection test: a rule
// stated in prose but enforced nowhere is the failure this package exists to
// prevent.
type Rule struct {
	ID    string
	Title string
	check func(*trace) []Violation
}

var rules = []Rule{
	{ID: "C0", Title: "The artifact decodes as a canonical version 1 JSONL stream"},
	{ID: "C1", Title: "A started debate has between two and ten participants", check: checkParticipantCount},
	{ID: "C2", Title: "An argument names a participant; a system message has no speaker", check: checkSpeakerIdentity},
	{ID: "C3", Title: "A vote inside a message supports a participant", check: checkSupportTarget},
	{ID: "C4", Title: "Arguments follow turn order and no participant speaks twice in a round", check: checkTurnOrder},
	{ID: "C5", Title: "Rounds run from one to the declared count and never move backwards", check: checkRounds},
	{ID: "C6", Title: "A round carries at most one structured summary", check: checkRoundSummaries},
	{ID: "C7", Title: "There is at most one verdict and it closes the transcript", check: checkVerdict},
	{ID: "C8", Title: "In moderator mode the debate agrees with its verdict", check: checkModeratorConsensus},
	{ID: "C9", Title: "In hybrid mode the votes decide and nothing may contradict them", check: checkHybridConsensus},
	{ID: "C10", Title: "Votes are a function of the transcript", check: checkVotesAreDerived},
	{ID: "C11", Title: "Votes appear only in a hybrid debate that has started", check: checkVotesAreAbsent},
	{ID: "C12", Title: "Every citation points backwards at a record of this transcript", check: checkCitations},
	{ID: "C13", Title: "The discussion context is withheld while the debate is open", check: checkEmbargo},
	{ID: "C14", Title: "Nothing is said before the first round begins", check: checkTranscriptIsEmptyBeforeStart},
}

// Rules returns the normative rules in SPEC.md order.
func Rules() []Rule { return append([]Rule(nil), rules...) }

// trace is one decoded artifact grouped the way the rules read it.
type trace struct {
	debate       protocol.DebateRecord
	participants []protocol.ParticipantRecord
	transcript   []protocol.ExportRecord
	votes        []protocol.VoteRecord
}

func split(records []protocol.ExportRecord) (*trace, []Violation) {
	if len(records) == 0 || records[0].RecordType != protocol.RecordDebate {
		return nil, []Violation{{Rule: "C0", Detail: "artifact does not start with a debate record"}}
	}
	artifact := &trace{debate: *records[0].Debate}
	for _, record := range records[1:] {
		switch record.RecordType {
		case protocol.RecordParticipant:
			artifact.participants = append(artifact.participants, *record.Participant)
		case protocol.RecordMessage, protocol.RecordRoundSummary, protocol.RecordVerdict:
			artifact.transcript = append(artifact.transcript, record)
		case protocol.RecordVote:
			artifact.votes = append(artifact.votes, *record.Vote)
		}
	}
	return artifact, nil
}

// started reports whether the debate has left the pre-turn statuses. Rules that
// speak about turns, rounds, or votes apply only from that point on.
func (t *trace) started() bool {
	return t.debate.Status != core.StatusOpen && t.debate.Status != core.StatusPreparing
}

func (t *trace) participantIDs() map[string]protocol.ParticipantRecord {
	byID := make(map[string]protocol.ParticipantRecord, len(t.participants))
	for _, participant := range t.participants {
		byID[participant.AgentID] = participant
	}
	return byID
}

// turnOrder returns participants in the order they take turns: by join time,
// ties broken by the opaque agent_id. The artifact lists them sorted by agent_id
// instead, so turn order is recovered here rather than read off the stream.
func (t *trace) turnOrder() []protocol.ParticipantRecord {
	ordered := append([]protocol.ParticipantRecord(nil), t.participants...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if !ordered[i].JoinedAt.Equal(ordered[j].JoinedAt) {
			return ordered[i].JoinedAt.Before(ordered[j].JoinedAt)
		}
		return ordered[i].AgentID < ordered[j].AgentID
	})
	return ordered
}

func transcriptSeq(record protocol.ExportRecord) int64 {
	switch record.RecordType {
	case protocol.RecordMessage:
		return record.Message.Seq
	case protocol.RecordRoundSummary:
		return record.RoundSummary.Seq
	case protocol.RecordVerdict:
		return record.Verdict.Seq
	}
	return 0
}

func transcriptRound(record protocol.ExportRecord) int {
	switch record.RecordType {
	case protocol.RecordMessage:
		return record.Message.Round
	case protocol.RecordRoundSummary:
		return record.RoundSummary.Round
	case protocol.RecordVerdict:
		return record.Verdict.Round
	}
	return 0
}

// C1: a debate that has started has between two and MaxParticipants
// participants, and the roster cannot grow after that.
func checkParticipantCount(t *trace) []Violation {
	var violations []Violation
	if count := len(t.participants); count > core.MaxParticipants {
		violations = append(violations, Violation{"C1",
			fmt.Sprintf("%d participants exceed the maximum of %d", count, core.MaxParticipants)})
	}
	if t.started() && len(t.participants) < 2 {
		violations = append(violations, Violation{"C1",
			fmt.Sprintf("status %q with %d participants; a started debate has at least two",
				t.debate.Status, len(t.participants))})
	}
	return violations
}

// C2: an argument names a participant as its speaker; a system message has no
// speaker and casts no vote.
func checkSpeakerIdentity(t *trace) []Violation {
	byID := t.participantIDs()
	var violations []Violation
	for _, record := range t.transcript {
		if record.RecordType != protocol.RecordMessage {
			continue
		}
		message := record.Message
		switch message.Kind {
		case core.KindArgument:
			if message.SpeakerID == "" {
				violations = append(violations, Violation{"C2",
					fmt.Sprintf("argument at seq %d has no speaker_id", message.Seq)})
				continue
			}
			if _, ok := byID[message.SpeakerID]; !ok {
				violations = append(violations, Violation{"C2",
					fmt.Sprintf("argument at seq %d is spoken by %q, who is not a participant",
						message.Seq, message.SpeakerID)})
			}
		case core.KindSystem:
			if message.SpeakerID != "" {
				violations = append(violations, Violation{"C2",
					fmt.Sprintf("system message at seq %d has speaker_id %q; system messages have no speaker",
						message.Seq, message.SpeakerID)})
			}
			if message.SupportID != "" {
				violations = append(violations, Violation{"C2",
					fmt.Sprintf("system message at seq %d carries support_id %q; only a participant votes",
						message.Seq, message.SupportID)})
			}
		}
	}
	return violations
}

// C3: a vote cast inside a message supports a participant of this debate.
func checkSupportTarget(t *trace) []Violation {
	byID := t.participantIDs()
	var violations []Violation
	for _, record := range t.transcript {
		if record.RecordType != protocol.RecordMessage || record.Message.SupportID == "" {
			continue
		}
		message := record.Message
		if _, ok := byID[message.SupportID]; !ok {
			violations = append(violations, Violation{"C3",
				fmt.Sprintf("message at seq %d supports %q, who is not a participant",
					message.Seq, message.SupportID)})
		}
	}
	return violations
}

// C4: within one round, arguments follow the turn order and no participant
// speaks twice. A participant who missed the turn is simply absent.
func checkTurnOrder(t *trace) []Violation {
	position := make(map[string]int, len(t.participants))
	for index, participant := range t.turnOrder() {
		position[participant.AgentID] = index
	}
	lastPosition := make(map[int]int)
	var violations []Violation
	for _, record := range t.transcript {
		if record.RecordType != protocol.RecordMessage || record.Message.Kind != core.KindArgument {
			continue
		}
		message := record.Message
		current, ok := position[message.SpeakerID]
		if !ok {
			continue // already reported by C2
		}
		previous, seen := lastPosition[message.Round]
		if seen && current <= previous {
			violations = append(violations, Violation{"C4",
				fmt.Sprintf("argument at seq %d speaks out of turn in round %d: %q holds turn position %d after position %d",
					message.Seq, message.Round, message.SpeakerID, current, previous)})
		}
		lastPosition[message.Round] = current
	}
	return violations
}

// C5: rounds run from one to the declared count, never move backwards along the
// transcript, and never run ahead of current_round.
func checkRounds(t *trace) []Violation {
	var violations []Violation
	if t.debate.CurrentRound > t.debate.Rounds {
		violations = append(violations, Violation{"C5",
			fmt.Sprintf("current_round %d exceeds the declared %d rounds", t.debate.CurrentRound, t.debate.Rounds)})
	}
	if t.started() != (t.debate.CurrentRound >= 1) {
		violations = append(violations, Violation{"C5",
			fmt.Sprintf("status %q with current_round %d; rounds start exactly when the debate leaves open and preparing",
				t.debate.Status, t.debate.CurrentRound)})
	}
	previous := 0
	for _, record := range t.transcript {
		round := transcriptRound(record)
		if round < 1 || round > t.debate.Rounds {
			violations = append(violations, Violation{"C5",
				fmt.Sprintf("record at seq %d is in round %d, outside 1..%d",
					transcriptSeq(record), round, t.debate.Rounds)})
		}
		if round < previous {
			violations = append(violations, Violation{"C5",
				fmt.Sprintf("record at seq %d moves back from round %d to round %d",
					transcriptSeq(record), previous, round)})
		}
		if round > t.debate.CurrentRound {
			violations = append(violations, Violation{"C5",
				fmt.Sprintf("record at seq %d is in round %d, ahead of current_round %d",
					transcriptSeq(record), round, t.debate.CurrentRound)})
		}
		previous = round
	}
	return violations
}

// C6: a round carries at most one structured summary. Two would leave a reader
// unable to say which one the debate acted on.
func checkRoundSummaries(t *trace) []Violation {
	seen := make(map[int]int64)
	var violations []Violation
	for _, record := range t.transcript {
		if record.RecordType != protocol.RecordRoundSummary || record.RoundSummary.Result == nil {
			continue
		}
		round := record.RoundSummary.Round
		if first, duplicate := seen[round]; duplicate {
			violations = append(violations, Violation{"C6",
				fmt.Sprintf("round %d has structured summaries at seq %d and seq %d",
					round, first, record.RoundSummary.Seq)})
			continue
		}
		seen[round] = record.RoundSummary.Seq
	}
	return violations
}

// C7: there is at most one verdict, it closes the transcript, and it cannot
// appear before the debate has reached moderation.
func checkVerdict(t *trace) []Violation {
	var violations []Violation
	var first *protocol.VerdictRecord
	for index, record := range t.transcript {
		if record.RecordType != protocol.RecordVerdict {
			continue
		}
		if first != nil {
			violations = append(violations, Violation{"C7",
				fmt.Sprintf("second verdict at seq %d; the first is at seq %d", record.Verdict.Seq, first.Seq)})
			continue
		}
		first = record.Verdict
		if index != len(t.transcript)-1 {
			violations = append(violations, Violation{"C7",
				fmt.Sprintf("verdict at seq %d is followed by %d more transcript records",
					record.Verdict.Seq, len(t.transcript)-index-1)})
		}
	}
	if first == nil {
		return violations
	}
	switch t.debate.Status {
	case core.StatusModerating, core.StatusConcluded:
	default:
		violations = append(violations, Violation{"C7",
			fmt.Sprintf("verdict at seq %d in status %q; a verdict exists only from moderation onwards",
				first.Seq, t.debate.Status)})
	}
	return violations
}

// C8: in moderator mode the verdict decides. A concluded debate that recorded
// one agrees with it, so a reader cannot be shown two different outcomes.
func checkModeratorConsensus(t *trace) []Violation {
	if t.debate.Mode != core.ModeModerator || t.debate.Status != core.StatusConcluded {
		return nil
	}
	verdict := t.structuredVerdict()
	if verdict == nil {
		return nil
	}
	if verdict.Consensus != t.debate.Consensus {
		return []Violation{{"C8",
			fmt.Sprintf("debate consensus is %t but the verdict says %t", t.debate.Consensus, verdict.Consensus)}}
	}
	return nil
}

// C9: in hybrid mode the participants' votes decide, and neither the debate
// record nor the model's verdict may report a different outcome.
func checkHybridConsensus(t *trace) []Violation {
	if t.debate.Mode != core.ModeHybrid || t.debate.Status != core.StatusConcluded {
		return nil
	}
	var violations []Violation
	if unanimous := unanimity(t.votes); unanimous != t.debate.Consensus {
		violations = append(violations, Violation{"C9",
			fmt.Sprintf("debate consensus is %t but the recorded votes are %t",
				t.debate.Consensus, unanimous)})
	}
	if verdict := t.structuredVerdict(); verdict != nil && verdict.Consensus != t.debate.Consensus {
		violations = append(violations, Violation{"C9",
			fmt.Sprintf("verdict consensus is %t but the debate consensus is %t",
				verdict.Consensus, t.debate.Consensus)})
	}
	return violations
}

func (t *trace) structuredVerdict() *core.ModerationVerdict {
	for _, record := range t.transcript {
		if record.RecordType == protocol.RecordVerdict && record.Verdict.Result != nil {
			return record.Verdict.Result
		}
	}
	return nil
}

// unanimity mirrors the hybrid consensus rule: every participant who spoke
// supports one position, and at least two of them did.
func unanimity(votes []protocol.VoteRecord) bool {
	if len(votes) < 2 {
		return false
	}
	for _, vote := range votes[1:] {
		if vote.SupportsID != votes[0].SupportsID {
			return false
		}
	}
	return true
}

// C10: votes are a function of the transcript, not an independent claim. Each
// participant who spoke votes for whoever their last argument supported, and a
// participant who never spoke has no vote. A reader can therefore recompute the
// vote block and detect a producer that fabricated it.
func checkVotesAreDerived(t *trace) []Violation {
	if t.debate.Mode != core.ModeHybrid || !t.started() {
		return nil
	}
	byID := t.participantIDs()
	expected := make(map[string]string, len(t.participants))
	for _, record := range t.transcript {
		if record.RecordType != protocol.RecordMessage || record.Message.Kind != core.KindArgument {
			continue
		}
		message := record.Message
		if message.SpeakerID == "" {
			continue
		}
		supported := message.SupportID
		if supported == "" {
			supported = message.SpeakerID // silence keeps a speaker on their own position
		}
		expected[message.SpeakerID] = supported
	}

	var violations []Violation
	recorded := make(map[string]protocol.VoteRecord, len(t.votes))
	for _, vote := range t.votes {
		recorded[vote.AgentID] = vote
		want, spoke := expected[vote.AgentID]
		if !spoke {
			violations = append(violations, Violation{"C10",
				fmt.Sprintf("%q has a vote but never spoke", vote.AgentID)})
			continue
		}
		if vote.SupportsID != want {
			violations = append(violations, Violation{"C10",
				fmt.Sprintf("%q votes for %q but their last argument supports %q",
					vote.AgentID, vote.SupportsID, want)})
		}
		if participant, ok := byID[vote.AgentID]; ok && vote.AgentName != participant.Name {
			violations = append(violations, Violation{"C10",
				fmt.Sprintf("vote of %q names them %q, the participant record names them %q",
					vote.AgentID, vote.AgentName, participant.Name)})
		}
		if participant, ok := byID[vote.SupportsID]; ok && vote.SupportsName != participant.Name {
			violations = append(violations, Violation{"C10",
				fmt.Sprintf("vote of %q names the supported participant %q, their record names them %q",
					vote.AgentID, vote.SupportsName, participant.Name)})
		} else if !ok {
			violations = append(violations, Violation{"C10",
				fmt.Sprintf("%q votes for %q, who is not a participant", vote.AgentID, vote.SupportsID)})
		}
	}
	for agentID := range expected {
		if _, ok := recorded[agentID]; !ok {
			violations = append(violations, Violation{"C10",
				fmt.Sprintf("%q spoke but has no vote", agentID)})
		}
	}
	sort.Slice(violations, func(i, j int) bool { return violations[i].Detail < violations[j].Detail })
	return violations
}

// C11: votes exist only where they mean something — a hybrid debate that has
// started. In moderator mode they are not the consensus mechanism, and before
// the start nobody has spoken.
func checkVotesAreAbsent(t *trace) []Violation {
	if len(t.votes) == 0 {
		return nil
	}
	if t.debate.Mode != core.ModeHybrid {
		return []Violation{{"C11",
			fmt.Sprintf("%d vote records in mode %q, where votes do not decide consensus",
				len(t.votes), t.debate.Mode)}}
	}
	// Именно !started(), а не «не open»: в preparing тоже никто не говорил, а
	// C10 в этом статусе не работает — значит без этой проверки выдуманный блок
	// голосов в подготовке проходил бы вообще без возражений.
	if !t.started() {
		return []Violation{{"C11",
			fmt.Sprintf("%d vote records in status %q, before anyone could speak", len(t.votes), t.debate.Status)}}
	}
	return nil
}

// C12: every citation points backwards at a record of this transcript, so a
// claim of the moderator can be checked against what was actually said.
func checkCitations(t *trace) []Violation {
	seqs := make(map[int64]struct{}, len(t.transcript))
	for _, record := range t.transcript {
		seqs[transcriptSeq(record)] = struct{}{}
	}
	var violations []Violation
	check := func(kind string, own int64, claims []core.ModerationClaim) {
		for index, claim := range claims {
			for _, citation := range claim.Citations {
				if _, ok := seqs[citation]; !ok {
					violations = append(violations, Violation{"C12",
						fmt.Sprintf("%s at seq %d cites seq %d in claims[%d], which is not in this transcript",
							kind, own, citation, index)})
					continue
				}
				if citation >= own {
					violations = append(violations, Violation{"C12",
						fmt.Sprintf("%s at seq %d cites seq %d in claims[%d], which it does not follow",
							kind, own, citation, index)})
				}
			}
		}
	}
	for _, record := range t.transcript {
		switch {
		case record.RecordType == protocol.RecordRoundSummary && record.RoundSummary.Result != nil:
			check("round summary", record.RoundSummary.Seq, record.RoundSummary.Result.Claims)
		case record.RecordType == protocol.RecordVerdict && record.Verdict.Result != nil:
			check("verdict", record.Verdict.Seq, record.Verdict.Result.Claims)
		}
	}
	return violations
}

// C13: the discussion context is withheld while the debate is open, so joining
// early buys no head start. The export is a disclosure surface like any other.
func checkEmbargo(t *trace) []Violation {
	if t.debate.Status == core.StatusOpen && t.debate.Description != "" {
		return []Violation{{"C13",
			"status is open but the debate record carries a description; the context is withheld until start"}}
	}
	return nil
}

// C14: nothing is said before the first round begins.
func checkTranscriptIsEmptyBeforeStart(t *trace) []Violation {
	if t.started() || len(t.transcript) == 0 {
		return nil
	}
	return []Violation{{"C14",
		fmt.Sprintf("status %q carries %d transcript records; turns start only after that status",
			t.debate.Status, len(t.transcript))}}
}
