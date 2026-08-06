// Package protocol defines the versioned wire records shared by export,
// golden traces, and future conformance consumers.
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"court/internal/core"
)

// RecordType identifies the payload carried by one JSONL export line.
type RecordType string

const (
	RecordDebate       RecordType = "debate"
	RecordParticipant  RecordType = "participant"
	RecordMessage      RecordType = "message"
	RecordVote         RecordType = "vote"
	RecordRoundSummary RecordType = "round_summary"
	RecordVerdict      RecordType = "verdict"
)

// ExportRecord is the canonical tagged union for one versioned JSONL line.
// Exactly one payload must be present and must match RecordType.
type ExportRecord struct {
	SchemaVersion int                 `json:"schema_version"`
	RecordType    RecordType          `json:"record_type"`
	DebateID      string              `json:"debate_id"`
	Debate        *DebateRecord       `json:"debate,omitempty"`
	Participant   *ParticipantRecord  `json:"participant,omitempty"`
	Message       *MessageRecord      `json:"message,omitempty"`
	Vote          *VoteRecord         `json:"vote,omitempty"`
	RoundSummary  *RoundSummaryRecord `json:"round_summary,omitempty"`
	Verdict       *VerdictRecord      `json:"verdict,omitempty"`
}

// Versioned sets the current schema version on a record being produced.
func (r ExportRecord) Versioned() ExportRecord {
	if r.SchemaVersion == 0 {
		r.SchemaVersion = core.CurrentProtocolSchemaVersion
	}
	return r
}

// MarshalJSON is the canonical producer boundary: it supplies v1 when omitted,
// rejects unsupported versions or tags, and normalizes every timestamp to UTC.
func (r ExportRecord) MarshalJSON() ([]byte, error) {
	type wireRecord ExportRecord
	r = r.Versioned()
	r.normalizeTimes()
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(wireRecord(r))
}

// Validate rejects ambiguous or mismatched tagged records before export.
func (r ExportRecord) Validate() error {
	if r.SchemaVersion != core.CurrentProtocolSchemaVersion {
		return fmt.Errorf("schema_version = %d, want %d", r.SchemaVersion, core.CurrentProtocolSchemaVersion)
	}
	if r.DebateID == "" {
		return fmt.Errorf("debate_id is required")
	}
	present := 0
	matched := false
	for recordType, exists := range map[RecordType]bool{
		RecordDebate:       r.Debate != nil,
		RecordParticipant:  r.Participant != nil,
		RecordMessage:      r.Message != nil,
		RecordVote:         r.Vote != nil,
		RecordRoundSummary: r.RoundSummary != nil,
		RecordVerdict:      r.Verdict != nil,
	} {
		if exists {
			present++
			matched = matched || r.RecordType == recordType
		}
	}
	if present != 1 {
		return fmt.Errorf("record must contain exactly one payload, got %d", present)
	}
	if !matched {
		return fmt.Errorf("record_type %q does not match its payload", r.RecordType)
	}
	if r.Debate != nil {
		if !knownMode(r.Debate.Mode) {
			return fmt.Errorf("unsupported debate mode %q in schema v%d", r.Debate.Mode, core.CurrentProtocolSchemaVersion)
		}
		if !knownStatus(r.Debate.Status) {
			return fmt.Errorf("unsupported debate status %q in schema v%d", r.Debate.Status, core.CurrentProtocolSchemaVersion)
		}
	}
	if r.Message != nil && !exportMessageKind(r.Message.Kind) {
		return fmt.Errorf("message kind %q requires a different record type in schema v%d", r.Message.Kind, core.CurrentProtocolSchemaVersion)
	}
	return nil
}

// RecordForMessage applies the one-to-one v1 mapping from durable transcript
// kind to export record type. Pre-v1 summaries and verdicts keep text and an
// absent Result instead of fabricating structured evidence.
func RecordForMessage(message core.Message) (ExportRecord, error) {
	record := ExportRecord{DebateID: message.DebateID}
	switch message.Kind {
	case core.KindArgument, core.KindSystem:
		if message.RoundSummary != nil || message.Verdict != nil || message.LegacyUnstructured {
			return ExportRecord{}, fmt.Errorf("message kind %q cannot carry structured moderation", message.Kind)
		}
		record.RecordType = RecordMessage
		record.Message = &MessageRecord{
			Seq: message.Seq, Round: message.Round, SpeakerID: message.SpeakerID,
			SpeakerName: message.SpeakerName, Kind: message.Kind, Text: message.Text,
			SupportID: message.SupportID, SupportName: message.SupportName, CreatedAt: message.CreatedAt,
		}
	case core.KindSummary:
		if message.Verdict != nil {
			return ExportRecord{}, errors.New("summary message cannot carry verdict payload")
		}
		if (message.RoundSummary == nil) != message.LegacyUnstructured {
			return ExportRecord{}, errors.New("summary must have typed result or explicit legacy marker")
		}
		record.RecordType = RecordRoundSummary
		record.RoundSummary = &RoundSummaryRecord{
			Seq: message.Seq, Round: message.Round, SpeakerName: message.SpeakerName,
			Text: message.Text, Result: message.RoundSummary, CreatedAt: message.CreatedAt,
		}
	case core.KindVerdict:
		if message.RoundSummary != nil {
			return ExportRecord{}, errors.New("verdict message cannot carry round_summary payload")
		}
		if (message.Verdict == nil) != message.LegacyUnstructured {
			return ExportRecord{}, errors.New("verdict must have typed result or explicit legacy marker")
		}
		record.RecordType = RecordVerdict
		record.Verdict = &VerdictRecord{
			Seq: message.Seq, Round: message.Round, SpeakerName: message.SpeakerName,
			Text: message.Text, Result: message.Verdict, CreatedAt: message.CreatedAt,
		}
	default:
		return ExportRecord{}, fmt.Errorf("unsupported message kind %q in schema v%d", message.Kind, core.CurrentProtocolSchemaVersion)
	}
	record = record.Versioned()
	if err := record.Validate(); err != nil {
		return ExportRecord{}, err
	}
	return record, nil
}

// CanonicalStream assembles the only supported v1 JSONL record order:
// debate metadata, participants by agent_id, the transcript by seq, then
// current votes by agent_id. Export and golden-trace producers share this
// boundary so independently valid lines cannot form nondeterministic streams.
func CanonicalStream(
	debate ExportRecord,
	participants []ExportRecord,
	transcript []ExportRecord,
	votes []ExportRecord,
) ([]ExportRecord, error) {
	debate = debate.Versioned()
	if debate.RecordType != RecordDebate {
		return nil, fmt.Errorf("canonical stream must start with one debate record")
	}
	if err := debate.Validate(); err != nil {
		return nil, fmt.Errorf("debate record: %w", err)
	}
	debateID := debate.DebateID

	participants = append([]ExportRecord(nil), participants...)
	transcript = append([]ExportRecord(nil), transcript...)
	votes = append([]ExportRecord(nil), votes...)
	if err := validateStreamGroup(debateID, participants, RecordParticipant); err != nil {
		return nil, fmt.Errorf("participants: %w", err)
	}
	if err := validateTranscriptGroup(debateID, transcript); err != nil {
		return nil, fmt.Errorf("transcript: %w", err)
	}
	if err := validateStreamGroup(debateID, votes, RecordVote); err != nil {
		return nil, fmt.Errorf("votes: %w", err)
	}

	sort.Slice(participants, func(i, j int) bool {
		return participants[i].Participant.AgentID < participants[j].Participant.AgentID
	})
	sort.Slice(transcript, func(i, j int) bool {
		return transcriptSeq(transcript[i]) < transcriptSeq(transcript[j])
	})
	sort.Slice(votes, func(i, j int) bool {
		return votes[i].Vote.AgentID < votes[j].Vote.AgentID
	})
	if err := rejectDuplicateStreamKeys(participants, func(r ExportRecord) string { return r.Participant.AgentID }); err != nil {
		return nil, fmt.Errorf("participants: %w", err)
	}
	if err := rejectDuplicateTranscriptSeqs(transcript); err != nil {
		return nil, fmt.Errorf("transcript: %w", err)
	}
	if err := rejectDuplicateStreamKeys(votes, func(r ExportRecord) string { return r.Vote.AgentID }); err != nil {
		return nil, fmt.Errorf("votes: %w", err)
	}

	result := make([]ExportRecord, 0, 1+len(participants)+len(transcript)+len(votes))
	result = append(result, debate)
	result = append(result, participants...)
	result = append(result, transcript...)
	result = append(result, votes...)
	return result, nil
}

func validateStreamGroup(debateID string, records []ExportRecord, want RecordType) error {
	for i := range records {
		records[i] = records[i].Versioned()
		if records[i].RecordType != want {
			return fmt.Errorf("record %d has type %q, want %q", i, records[i].RecordType, want)
		}
		if records[i].DebateID != debateID {
			return fmt.Errorf("record %d has debate_id %q, want %q", i, records[i].DebateID, debateID)
		}
		if err := records[i].Validate(); err != nil {
			return fmt.Errorf("record %d: %w", i, err)
		}
	}
	return nil
}

func validateTranscriptGroup(debateID string, records []ExportRecord) error {
	for i := range records {
		records[i] = records[i].Versioned()
		switch records[i].RecordType {
		case RecordMessage, RecordRoundSummary, RecordVerdict:
		default:
			return fmt.Errorf("record %d has non-transcript type %q", i, records[i].RecordType)
		}
		if records[i].DebateID != debateID {
			return fmt.Errorf("record %d has debate_id %q, want %q", i, records[i].DebateID, debateID)
		}
		if err := records[i].Validate(); err != nil {
			return fmt.Errorf("record %d: %w", i, err)
		}
	}
	return nil
}

func transcriptSeq(record ExportRecord) int64 {
	switch record.RecordType {
	case RecordMessage:
		return record.Message.Seq
	case RecordRoundSummary:
		return record.RoundSummary.Seq
	case RecordVerdict:
		return record.Verdict.Seq
	default:
		return 0
	}
}

func rejectDuplicateStreamKeys(records []ExportRecord, key func(ExportRecord) string) error {
	for i := 1; i < len(records); i++ {
		if key(records[i-1]) == key(records[i]) {
			return fmt.Errorf("duplicate key %q", key(records[i]))
		}
	}
	return nil
}

func rejectDuplicateTranscriptSeqs(records []ExportRecord) error {
	for i := 1; i < len(records); i++ {
		if transcriptSeq(records[i-1]) == transcriptSeq(records[i]) {
			return fmt.Errorf("duplicate seq %d", transcriptSeq(records[i]))
		}
	}
	return nil
}

func (r *ExportRecord) normalizeTimes() {
	if r.Debate != nil {
		value := *r.Debate
		value.CreatedAt = utc(value.CreatedAt)
		r.Debate = &value
	}
	if r.Participant != nil {
		value := *r.Participant
		value.JoinedAt = utc(value.JoinedAt)
		r.Participant = &value
	}
	if r.Message != nil {
		value := *r.Message
		value.CreatedAt = utc(value.CreatedAt)
		r.Message = &value
	}
	if r.RoundSummary != nil {
		value := *r.RoundSummary
		value.CreatedAt = utc(value.CreatedAt)
		r.RoundSummary = &value
	}
	if r.Verdict != nil {
		value := *r.Verdict
		value.CreatedAt = utc(value.CreatedAt)
		r.Verdict = &value
	}
}

func utc(value time.Time) time.Time {
	if value.IsZero() {
		return value
	}
	return value.UTC()
}

func knownMode(mode core.DebateMode) bool {
	return mode == core.ModeModerator || mode == core.ModeHybrid
}

func knownStatus(status core.DebateStatus) bool {
	switch status {
	case core.StatusOpen, core.StatusPreparing, core.StatusRunning, core.StatusModerating, core.StatusConcluded:
		return true
	default:
		return false
	}
}

func exportMessageKind(kind string) bool {
	return kind == core.KindArgument || kind == core.KindSystem
}

// DebateRecord captures the durable debate state required to replay a trace.
type DebateRecord struct {
	Question       string            `json:"question"`
	Description    string            `json:"description,omitempty"`
	Mode           core.DebateMode   `json:"mode"`
	Status         core.DebateStatus `json:"status"`
	Rounds         int               `json:"rounds"`
	CurrentRound   int               `json:"current_round"`
	TurnTimeoutSec int               `json:"turn_timeout_sec"`
	PrepTimeSec    int               `json:"prep_time_sec,omitempty"`
	CreatorID      string            `json:"creator_id"`
	Consensus      bool              `json:"consensus"`
	CreatedAt      time.Time         `json:"created_at"`
}

// ParticipantRecord keeps stable identity plus optional evaluation metadata.
type ParticipantRecord struct {
	AgentID       string    `json:"agent_id"`
	Name          string    `json:"name"`
	Persona       string    `json:"persona,omitempty"`
	Stance        string    `json:"stance,omitempty"`
	Model         string    `json:"model,omitempty"`
	PromptVersion string    `json:"prompt_version,omitempty"`
	JoinedAt      time.Time `json:"joined_at"`
}

// MessageRecord is an argument or system message in transcript order.
// Structured moderator records use RoundSummaryRecord or VerdictRecord.
type MessageRecord struct {
	Seq         int64     `json:"seq"`
	Round       int       `json:"round"`
	SpeakerID   string    `json:"speaker_id,omitempty"`
	SpeakerName string    `json:"speaker_name"`
	Kind        string    `json:"kind"`
	Text        string    `json:"text"`
	SupportID   string    `json:"support_id,omitempty"`
	SupportName string    `json:"support_name,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// VoteRecord is one participant's latest vote at export time.
type VoteRecord struct {
	AgentID      string `json:"agent_id"`
	AgentName    string `json:"agent_name"`
	SupportsID   string `json:"supports_id"`
	SupportsName string `json:"supports_name"`
}

// ExecutionMetadata is optional until providers expose usage consistently.
// Integer micro-USD avoids floating-point ambiguity; pointers distinguish an
// unknown measurement from a measured zero.
type ExecutionMetadata struct {
	Provider      string `json:"provider,omitempty"`
	Model         string `json:"model,omitempty"`
	PromptVersion string `json:"prompt_version,omitempty"`
	InputTokens   *int64 `json:"input_tokens,omitempty"`
	OutputTokens  *int64 `json:"output_tokens,omitempty"`
	CostMicroUSD  *int64 `json:"cost_micro_usd,omitempty"`
	LatencyMS     *int64 `json:"latency_ms,omitempty"`
}

// RoundSummaryRecord preserves the typed result and its ordered transcript
// position. Text remains for compatibility with current readers.
type RoundSummaryRecord struct {
	Seq         int64              `json:"seq"`
	Round       int                `json:"round"`
	SpeakerName string             `json:"speaker_name"`
	Text        string             `json:"text"`
	Result      *core.RoundSummary `json:"result,omitempty"`
	Execution   *ExecutionMetadata `json:"execution,omitempty"`
	CreatedAt   time.Time          `json:"created_at"`
}

// VerdictRecord is the final typed decision at its transcript position.
type VerdictRecord struct {
	Seq         int64                   `json:"seq"`
	Round       int                     `json:"round"`
	SpeakerName string                  `json:"speaker_name"`
	Text        string                  `json:"text"`
	Result      *core.ModerationVerdict `json:"result,omitempty"`
	Execution   *ExecutionMetadata      `json:"execution,omitempty"`
	CreatedAt   time.Time               `json:"created_at"`
}
