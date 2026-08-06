package protocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"court/internal/core"
)

func TestEveryEventKindSerializesCurrentSchemaVersion(t *testing.T) {
	for _, eventType := range []string{
		core.EventJoined,
		core.EventStarted,
		core.EventTurn,
		core.EventMessage,
		core.EventSkipped,
		core.EventConcluded,
		core.EventDeleted,
	} {
		t.Run(eventType, func(t *testing.T) {
			data, err := json.Marshal(core.Event{Type: eventType, DebateID: "dbt_test"})
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got struct {
				SchemaVersion int    `json:"schema_version"`
				Type          string `json:"type"`
				DebateID      string `json:"debate_id"`
			}
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.SchemaVersion != core.CurrentProtocolSchemaVersion || got.Type != eventType || got.DebateID != "dbt_test" {
				t.Fatalf("serialized event = %+v", got)
			}
			// The pre-v1 decoder shape has no schema_version field. Its ordinary
			// JSON decoding behavior ignores the additive field and preserves the
			// event tag and identity used by existing clients.
			var legacy struct {
				Type     string `json:"type"`
				DebateID string `json:"debate_id"`
			}
			if err := json.Unmarshal(data, &legacy); err != nil {
				t.Fatalf("legacy Unmarshal: %v", err)
			}
			if legacy.Type != eventType || legacy.DebateID != "dbt_test" {
				t.Fatalf("legacy event = %+v", legacy)
			}
		})
	}
}

func TestEventSerializationRejectsNonV1TagsAndNormalizesUTC(t *testing.T) {
	zone := time.FixedZone("UTC+3", 3*60*60)
	local := time.Date(2026, 8, 6, 12, 0, 0, 0, zone)
	data, err := json.Marshal(core.Event{
		Type: core.EventMessage, DebateID: "dbt_test", Deadline: local,
		Message: &core.Message{Kind: core.KindArgument, CreatedAt: local},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	jsonEvent := string(data)
	if !strings.Contains(jsonEvent, `"deadline":"2026-08-06T09:00:00Z"`) ||
		!strings.Contains(jsonEvent, `"created_at":"2026-08-06T09:00:00Z"`) {
		t.Fatalf("event timestamps were not normalized to UTC: %s", jsonEvent)
	}
	if _, err := json.Marshal(core.Event{
		SchemaVersion: core.CurrentProtocolSchemaVersion + 1, Type: core.EventStarted, DebateID: "dbt_test",
	}); err == nil {
		t.Fatal("Marshal accepted a non-v1 event schema version")
	}
	if _, err := json.Marshal(core.Event{Type: "future_event", DebateID: "dbt_test"}); err == nil {
		t.Fatal("Marshal accepted a new event tag inside schema v1")
	}
	if _, err := json.Marshal(core.Event{
		Type: core.EventMessage, DebateID: "dbt_test",
		Message: &core.Message{Kind: "future_message"},
	}); err == nil {
		t.Fatal("Marshal accepted a new nested message kind inside schema v1")
	}
}

func TestExportRecordIsVersionedAndRejectsAmbiguousPayloads(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	record := ExportRecord{
		RecordType: RecordDebate,
		DebateID:   "dbt_test",
		Debate: &DebateRecord{
			Question: "Question", Mode: core.ModeModerator, Status: core.StatusConcluded,
			Rounds: 2, CurrentRound: 1, TurnTimeoutSec: 60, CreatorID: "agt_owner",
			Consensus: true, CreatedAt: now,
		},
	}
	// Producers cannot forget the version: the serialization boundary supplies
	// it before validating the tagged union.
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	jsonLine := string(data)
	for _, required := range []string{
		`"schema_version":1`,
		`"record_type":"debate"`,
		`"debate_id":"dbt_test"`,
		`"debate":{`,
	} {
		if !strings.Contains(jsonLine, required) {
			t.Fatalf("JSONL record %s does not contain %s", jsonLine, required)
		}
	}

	versioned := record.Versioned()
	if err := versioned.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	ambiguous := versioned
	ambiguous.Message = &MessageRecord{Seq: 1, Kind: core.KindArgument, CreatedAt: now}
	if err := ambiguous.Validate(); err == nil {
		t.Fatal("Validate accepted two payloads")
	}
	if _, err := json.Marshal(ambiguous); err == nil {
		t.Fatal("Marshal accepted two payloads")
	}
	mismatched := versioned
	mismatched.RecordType = RecordVerdict
	if err := mismatched.Validate(); err == nil {
		t.Fatal("Validate accepted a record_type that does not match its payload")
	}
	unsupported := versioned
	unsupported.SchemaVersion++
	if _, err := json.Marshal(unsupported); err == nil {
		t.Fatal("Marshal accepted a non-v1 export schema version")
	}
}

func TestExportSerializationNormalizesEveryTimestampToUTC(t *testing.T) {
	zone := time.FixedZone("UTC+3", 3*60*60)
	local := time.Date(2026, 8, 6, 12, 0, 0, 0, zone)
	records := []ExportRecord{
		{RecordType: RecordDebate, DebateID: "dbt_test", Debate: &DebateRecord{
			Question: "Q", Mode: core.ModeModerator, Status: core.StatusConcluded,
			Rounds: 1, TurnTimeoutSec: 60, CreatorID: "agt", CreatedAt: local,
		}},
		{RecordType: RecordParticipant, DebateID: "dbt_test",
			Participant: &ParticipantRecord{AgentID: "agt", Name: "A", JoinedAt: local}},
		{RecordType: RecordMessage, DebateID: "dbt_test",
			Message: &MessageRecord{Seq: 1, Kind: core.KindArgument, Text: "A", CreatedAt: local}},
		{RecordType: RecordRoundSummary, DebateID: "dbt_test",
			RoundSummary: &RoundSummaryRecord{Seq: 2, Result: &core.RoundSummary{}, CreatedAt: local}},
		{RecordType: RecordVerdict, DebateID: "dbt_test",
			Verdict: &VerdictRecord{Seq: 3, Result: &core.ModerationVerdict{}, CreatedAt: local}},
	}
	for _, record := range records {
		data, err := json.Marshal(record)
		if err != nil {
			t.Fatalf("Marshal(%s): %v", record.RecordType, err)
		}
		if !strings.Contains(string(data), `"2026-08-06T09:00:00Z"`) {
			t.Fatalf("%s timestamp was not normalized to UTC: %s", record.RecordType, data)
		}
	}
}

func TestV1RejectsNewEnumAndTagValues(t *testing.T) {
	unknownMode := ExportRecord{
		RecordType: RecordDebate, DebateID: "dbt_test",
		Debate: &DebateRecord{Question: "Q", Mode: "future", Status: core.StatusOpen},
	}
	if _, err := json.Marshal(unknownMode); err == nil {
		t.Fatal("Marshal accepted a new debate mode inside schema v1")
	}
	unknownKind := ExportRecord{
		RecordType: RecordMessage, DebateID: "dbt_test",
		Message: &MessageRecord{Seq: 1, Kind: "future"},
	}
	if _, err := json.Marshal(unknownKind); err == nil {
		t.Fatal("Marshal accepted a new message kind inside schema v1")
	}
	for _, structuredKind := range []string{core.KindSummary, core.KindVerdict} {
		plainStructured := ExportRecord{
			RecordType: RecordMessage, DebateID: "dbt_test",
			Message: &MessageRecord{Seq: 1, Kind: structuredKind},
		}
		if _, err := json.Marshal(plainStructured); err == nil {
			t.Fatalf("Marshal accepted %q as a lossy plain message record", structuredKind)
		}
	}
	unknownRecord := ExportRecord{
		SchemaVersion: core.CurrentProtocolSchemaVersion,
		RecordType:    "future",
		DebateID:      "dbt_test",
		Message:       &MessageRecord{Seq: 1, Kind: core.KindArgument},
	}
	if err := unknownRecord.Validate(); err == nil {
		t.Fatal("Validate accepted a new record_type inside schema v1")
	}
}

func TestModerationExportSchemaKeepsCitationsAndUnknownUsageDistinctFromZero(t *testing.T) {
	zero := int64(0)
	record := ExportRecord{
		SchemaVersion: core.CurrentProtocolSchemaVersion,
		RecordType:    RecordRoundSummary,
		DebateID:      "dbt_test",
		RoundSummary: &RoundSummaryRecord{
			Seq: 3, Round: 1, SpeakerName: "Moderator", Text: "Summary",
			Result: &core.RoundSummary{
				Summary:             "Summary",
				Claims:              []core.ModerationClaim{{Text: "Claim", Citations: []int64{1, 2}}},
				UnresolvedQuestions: []string{}, Decisions: []string{"Decision"}, Consensus: true,
			},
			Execution: &ExecutionMetadata{Model: "model", CostMicroUSD: &zero},
			CreatedAt: time.Date(2026, 8, 6, 12, 1, 0, 0, time.UTC),
		},
	}
	if err := record.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	jsonLine := string(data)
	if !strings.Contains(jsonLine, `"citations":[1,2]`) {
		t.Fatalf("citations missing from %s", jsonLine)
	}
	if !strings.Contains(jsonLine, `"cost_micro_usd":0`) {
		t.Fatalf("measured zero cost missing from %s", jsonLine)
	}
	if strings.Contains(jsonLine, `"latency_ms"`) {
		t.Fatalf("unknown latency must be omitted from %s", jsonLine)
	}
}

func TestRecordForMessageMapsEachTranscriptSeqToOneCanonicalRecord(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 2, 0, 0, time.UTC)
	summary := core.RoundSummary{Summary: "Summary", Claims: []core.ModerationClaim{{Text: "Claim", Citations: []int64{1}}}}
	verdict := core.ModerationVerdict{FinalAnswer: "Answer", Consensus: true}
	messages := []core.Message{
		{Seq: 1, DebateID: "dbt_test", Kind: core.KindArgument, Text: "Argument", CreatedAt: now},
		{Seq: 2, DebateID: "dbt_test", Kind: core.KindSystem, Text: "System", CreatedAt: now},
		{Seq: 3, DebateID: "dbt_test", Kind: core.KindSummary, Text: summary.Text(), RoundSummary: &summary, CreatedAt: now},
		{Seq: 4, DebateID: "dbt_test", Kind: core.KindVerdict, Text: verdict.Text(), Verdict: &verdict, CreatedAt: now},
	}
	wantTypes := []RecordType{RecordMessage, RecordMessage, RecordRoundSummary, RecordVerdict}
	seen := make(map[int64]bool)
	for i, message := range messages {
		record, err := RecordForMessage(message)
		if err != nil {
			t.Fatalf("RecordForMessage(%s): %v", message.Kind, err)
		}
		if record.RecordType != wantTypes[i] {
			t.Fatalf("kind %q mapped to %q, want %q", message.Kind, record.RecordType, wantTypes[i])
		}
		var seq int64
		switch record.RecordType {
		case RecordMessage:
			seq = record.Message.Seq
		case RecordRoundSummary:
			seq = record.RoundSummary.Seq
			if record.RoundSummary.Result == nil || len(record.RoundSummary.Result.Claims) != 1 {
				t.Fatalf("structured summary lost: %+v", record.RoundSummary)
			}
		case RecordVerdict:
			seq = record.Verdict.Seq
			if record.Verdict.Result == nil || !record.Verdict.Result.Consensus {
				t.Fatalf("structured verdict lost: %+v", record.Verdict)
			}
		}
		if seen[seq] {
			t.Fatalf("transcript seq %d was mapped more than once", seq)
		}
		seen[seq] = true
		if _, err := json.Marshal(record); err != nil {
			t.Fatalf("Marshal mapped record %d: %v", seq, err)
		}
	}
	if len(seen) != len(messages) {
		t.Fatalf("mapped seq count = %d, want %d", len(seen), len(messages))
	}

	legacy := core.Message{
		Seq: 5, DebateID: "dbt_test", Kind: core.KindSummary, Text: "legacy prose",
		LegacyUnstructured: true, CreatedAt: now,
	}
	record, err := RecordForMessage(legacy)
	if err != nil {
		t.Fatalf("RecordForMessage legacy summary: %v", err)
	}
	if record.RecordType != RecordRoundSummary || record.RoundSummary.Result != nil {
		t.Fatalf("legacy summary mapping fabricated evidence: %+v", record)
	}

	for name, invalid := range map[string]core.Message{
		"unmarked legacy summary": {DebateID: "dbt_test", Kind: core.KindSummary},
		"argument with summary":   {DebateID: "dbt_test", Kind: core.KindArgument, RoundSummary: &summary},
		"system with verdict":     {DebateID: "dbt_test", Kind: core.KindSystem, Verdict: &verdict},
		"summary with verdict":    {DebateID: "dbt_test", Kind: core.KindSummary, Verdict: &verdict},
		"verdict with summary":    {DebateID: "dbt_test", Kind: core.KindVerdict, RoundSummary: &summary},
		"both structured":         {DebateID: "dbt_test", Kind: core.KindSummary, RoundSummary: &summary, Verdict: &verdict},
		"typed marked legacy":     {DebateID: "dbt_test", Kind: core.KindSummary, RoundSummary: &summary, LegacyUnstructured: true},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := RecordForMessage(invalid); err == nil {
				t.Fatal("RecordForMessage accepted inconsistent structured evidence")
			}
		})
	}
}

func TestCanonicalStreamHasDeterministicCrossRecordOrder(t *testing.T) {
	debate := ExportRecord{RecordType: RecordDebate, DebateID: "dbt_test", Debate: &DebateRecord{
		Question: "Q", Mode: core.ModeModerator, Status: core.StatusConcluded,
		Rounds: 1, TurnTimeoutSec: 60, CreatorID: "agt_b",
	}}
	participants := []ExportRecord{
		{RecordType: RecordParticipant, DebateID: "dbt_test", Participant: &ParticipantRecord{AgentID: "agt_b", Name: "B"}},
		{RecordType: RecordParticipant, DebateID: "dbt_test", Participant: &ParticipantRecord{AgentID: "agt_a", Name: "A"}},
	}
	transcript := []ExportRecord{
		{RecordType: RecordVerdict, DebateID: "dbt_test", Verdict: &VerdictRecord{Seq: 3, Result: &core.ModerationVerdict{FinalAnswer: "A"}}},
		{RecordType: RecordMessage, DebateID: "dbt_test", Message: &MessageRecord{Seq: 1, Kind: core.KindArgument, Text: "A"}},
		{RecordType: RecordRoundSummary, DebateID: "dbt_test", RoundSummary: &RoundSummaryRecord{Seq: 2, Result: &core.RoundSummary{Summary: "S"}}},
	}
	votes := []ExportRecord{
		{RecordType: RecordVote, DebateID: "dbt_test", Vote: &VoteRecord{AgentID: "agt_b", SupportsID: "agt_b"}},
		{RecordType: RecordVote, DebateID: "dbt_test", Vote: &VoteRecord{AgentID: "agt_a", SupportsID: "agt_a"}},
	}

	first, err := CanonicalStream(debate, participants, transcript, votes)
	if err != nil {
		t.Fatalf("CanonicalStream: %v", err)
	}
	second, err := CanonicalStream(debate,
		[]ExportRecord{participants[1], participants[0]},
		[]ExportRecord{transcript[1], transcript[2], transcript[0]},
		[]ExportRecord{votes[1], votes[0]},
	)
	if err != nil {
		t.Fatalf("CanonicalStream permuted: %v", err)
	}
	marshalJSONL := func(records []ExportRecord) string {
		t.Helper()
		var lines strings.Builder
		for _, record := range records {
			data, err := json.Marshal(record)
			if err != nil {
				t.Fatalf("Marshal(%s): %v", record.RecordType, err)
			}
			lines.Write(data)
			lines.WriteByte('\n')
		}
		return lines.String()
	}
	if got, want := marshalJSONL(second), marshalJSONL(first); got != want {
		t.Fatalf("canonical JSONL differs by input order:\n%s\nwant:\n%s", got, want)
	}
	wantTypes := []RecordType{
		RecordDebate, RecordParticipant, RecordParticipant,
		RecordMessage, RecordRoundSummary, RecordVerdict,
		RecordVote, RecordVote,
	}
	for i, want := range wantTypes {
		if first[i].RecordType != want {
			t.Fatalf("record %d type = %q, want %q", i, first[i].RecordType, want)
		}
	}
	if first[1].Participant.AgentID != "agt_a" || first[2].Participant.AgentID != "agt_b" ||
		first[6].Vote.AgentID != "agt_a" || first[7].Vote.AgentID != "agt_b" {
		t.Fatalf("participant/vote ordering is not by agent_id: %+v", first)
	}

	duplicateTranscript := append([]ExportRecord(nil), transcript...)
	duplicateTranscript[0].Verdict.Seq = 1
	if _, err := CanonicalStream(debate, participants, duplicateTranscript, votes); err == nil {
		t.Fatal("CanonicalStream accepted duplicate transcript seq")
	}
}
