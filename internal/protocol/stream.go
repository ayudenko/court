package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"

	"court/internal/core"
)

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
