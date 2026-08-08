package protocol

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"court/internal/core"
)

// decodeLineLimit bounds one JSONL line while reading an artifact produced
// elsewhere. An argument is capped at core.MaxArgumentLen characters, so a
// megabyte leaves room for escaping and for the moderation payload around it.
// The assumption this rests on is that no legitimate record exceeds it; a
// moderation result large enough to would make the server's own export
// undecodable, and would be a defect in the moderator's output bound.
const decodeLineLimit = 1024 * 1024

// MaxArtifactBytes bounds a whole artifact on read. The line limit alone bounds
// nothing: a stream of minimal valid lines costs several times its own size in
// decoded records and the slices Canonicalize copies, so without this a hostile
// producer sets the memory a reader spends.
//
// The value is the declared worst-case artifact the export endpoint is sized
// for. An artifact larger than anything this protocol can legitimately produce
// is refused rather than parsed.
const MaxArtifactBytes = 16 << 20

// Stream converts one consistent debate snapshot into the canonical v1 record
// stream. It is the single producer of that stream: the export endpoint and the
// golden-trace generator both call it, so a checked-in fixture cannot attest to
// bytes the server does not emit (docs/adr/0006-debate-export-endpoint.md).
//
// The snapshot carries the debate view rather than storage rows, so anything
// the public view withholds — the description of a debate that has not started
// — stays withheld here too.
func Stream(snapshot core.ExportSnapshot) ([]ExportRecord, error) {
	view := snapshot.Debate
	debate := ExportRecord{
		RecordType: RecordDebate,
		DebateID:   view.ID,
		Debate: &DebateRecord{
			Question: view.Question, Description: view.Description, Mode: view.Mode, Status: view.Status,
			Rounds: view.Rounds, CurrentRound: view.CurrentRound, TurnTimeoutSec: view.TurnTimeout,
			PrepTimeSec: view.PrepTime, CreatorID: view.CreatorID, Consensus: view.Consensus,
			CreatedAt: view.CreatedAt,
		},
	}

	agentsByID := make(map[string]core.Agent, len(snapshot.Participants))
	for _, agent := range snapshot.Participants {
		agentsByID[agent.ID] = agent
	}
	participants := make([]ExportRecord, 0, len(view.Participants))
	for _, participant := range view.Participants {
		agent, ok := agentsByID[participant.AgentID]
		if !ok {
			return nil, fmt.Errorf("participant %q has no agent metadata", participant.AgentID)
		}
		participants = append(participants, ExportRecord{
			RecordType: RecordParticipant,
			DebateID:   view.ID,
			Participant: &ParticipantRecord{
				AgentID: participant.AgentID, Name: participant.Name, Persona: agent.Persona,
				Stance: participant.Stance, JoinedAt: participant.JoinedAt,
			},
		})
	}

	transcript := make([]ExportRecord, 0, len(snapshot.Messages))
	for _, message := range snapshot.Messages {
		record, err := RecordForMessage(message)
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", message.Seq, err)
		}
		transcript = append(transcript, record)
	}

	votes := make([]ExportRecord, 0, len(view.Votes))
	for _, vote := range view.Votes {
		votes = append(votes, ExportRecord{
			RecordType: RecordVote,
			DebateID:   view.ID,
			Vote: &VoteRecord{
				AgentID: vote.AgentID, AgentName: vote.AgentName,
				SupportsID: vote.SupportsID, SupportsName: vote.SupportsName,
			},
		})
	}
	return CanonicalStream(debate, participants, transcript, votes)
}

// DecodeJSONL is the reader half of MarshalJSONL: it validates every line and
// returns the records in canonical order regardless of the order they arrived
// in. Re-encoding the result reproduces the canonical representation, so this
// is the entry point for reading an artifact produced by another implementation
// as well as for replaying a checked-in trace.
func DecodeJSONL(data []byte) ([]ExportRecord, error) {
	records, err := DecodeRecords(data)
	if err != nil {
		return nil, err
	}
	return Canonicalize(records)
}

// DecodeRecords validates every line and returns the records in the order the
// artifact presented them. Use it instead of DecodeJSONL when that order is
// itself the subject — a caller judging whether a producer emitted canonical
// order cannot use a reader that silently repairs it.
func DecodeRecords(data []byte) ([]ExportRecord, error) {
	if len(data) > MaxArtifactBytes {
		return nil, fmt.Errorf("artifact is %d bytes, over the %d-byte limit", len(data), MaxArtifactBytes)
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), decodeLineLimit)
	var records []ExportRecord
	for line := 1; scanner.Scan(); line++ {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			return nil, fmt.Errorf("line %d: blank JSONL record", line)
		}
		var record ExportRecord
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
	return records, nil
}

// Canonicalize groups already validated records and returns them in canonical
// order. It is the stream-level half of validation — one leading debate record,
// no stray second one, unique keys within each group — which per-record
// Validate cannot see. Any consumer holding records it did not obtain from
// DecodeJSONL must come through here before trusting their shape.
func Canonicalize(records []ExportRecord) ([]ExportRecord, error) {
	if len(records) == 0 {
		return nil, errors.New("trace contains no records")
	}
	debate := records[0]
	var participants, transcript, votes []ExportRecord
	for index, record := range records[1:] {
		switch record.RecordType {
		case RecordParticipant:
			participants = append(participants, record)
		case RecordMessage, RecordRoundSummary, RecordVerdict:
			transcript = append(transcript, record)
		case RecordVote:
			votes = append(votes, record)
		default:
			return nil, fmt.Errorf("record %d: unexpected type %q after debate", index+2, record.RecordType)
		}
	}
	return CanonicalStream(debate, participants, transcript, votes)
}

// MarshalJSONL serializes validated records as one JSON object per line.
// HTML escaping stays off so exported argument text reads as it was written.
func MarshalJSONL(records []ExportRecord) ([]byte, error) {
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
