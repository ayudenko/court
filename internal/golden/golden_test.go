package golden

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"court/internal/protocol"
)

func TestGenerateMatchesCheckedInGoldenTraces(t *testing.T) {
	artifacts, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(artifacts) != len(scenarios) {
		t.Fatalf("artifact count = %d, want %d", len(artifacts), len(scenarios))
	}
	manifest, err := os.ReadFile(filepath.Join("testdata", ".golden-manifest"))
	if err != nil {
		t.Fatalf("read ownership manifest: %v", err)
	}
	var artifactNames []string
	for _, artifact := range artifacts {
		artifactNames = append(artifactNames, artifact.Name)
	}
	sort.Strings(artifactNames)
	wantManifest := strings.Join(artifactNames, "\n") + "\n"
	if string(manifest) != wantManifest {
		t.Fatalf("ownership manifest is stale; run make golden\ngot:\n%s\nwant:\n%s", manifest, wantManifest)
	}
	for _, artifact := range artifacts {
		t.Run(artifact.Name, func(t *testing.T) {
			checkedIn, err := os.ReadFile(filepath.Join("testdata", artifact.Name))
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if !bytes.Equal(artifact.Data, checkedIn) {
				t.Fatalf("generated trace differs from %s; run make golden", artifact.Name)
			}

			replayed, err := protocol.DecodeJSONL(checkedIn)
			if err != nil {
				t.Fatalf("DecodeJSONL: %v", err)
			}
			roundTrip, err := protocol.MarshalJSONL(replayed)
			if err != nil {
				t.Fatalf("MarshalJSONL: %v", err)
			}
			if !bytes.Equal(roundTrip, checkedIn) {
				t.Fatal("replayed trace did not round-trip byte-for-byte")
			}
		})
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	first, err := Generate()
	if err != nil {
		t.Fatalf("first Generate: %v", err)
	}
	second, err := Generate()
	if err != nil {
		t.Fatalf("second Generate: %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("artifact count changed from %d to %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Name != second[i].Name || !bytes.Equal(first[i].Data, second[i].Data) {
			t.Fatalf("generation %d differs for artifact %q", i+1, first[i].Name)
		}
	}
}

func TestReplayCanonicalizesRecordOrder(t *testing.T) {
	artifacts, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var trace []byte
	for _, artifact := range artifacts {
		if artifact.Name == "hybrid_split_vote_v1.jsonl" {
			trace = artifact.Data
		}
	}
	if trace == nil {
		t.Fatal("hybrid_split_vote_v1.jsonl is missing; this test permutes its records")
	}
	lines := strings.Split(strings.TrimSuffix(string(trace), "\n"), "\n")
	if len(lines) != 8 {
		t.Fatalf("hybrid trace line count = %d, want 8", len(lines))
	}
	permuted := strings.Join([]string{
		lines[0], lines[2], lines[1], lines[4], lines[3], lines[5], lines[7], lines[6],
	}, "\n") + "\n"

	replayed, err := protocol.DecodeJSONL([]byte(permuted))
	if err != nil {
		t.Fatalf("DecodeJSONL: %v", err)
	}
	canonical, err := protocol.MarshalJSONL(replayed)
	if err != nil {
		t.Fatalf("MarshalJSONL: %v", err)
	}
	if !bytes.Equal(canonical, trace) {
		t.Fatal("replay did not restore canonical participant, transcript, and vote order")
	}
}

func TestReplayRejectsMissingSchemaVersion(t *testing.T) {
	trace := []byte(`{"record_type":"debate","debate_id":"dbt_test","debate":{"question":"Q","mode":"moderator","status":"open","rounds":1,"current_round":0,"turn_timeout_sec":30,"creator_id":"agt_test","consensus":false,"created_at":"2026-08-06T12:00:00Z"}}` + "\n")
	if _, err := protocol.DecodeJSONL(trace); err == nil || !strings.Contains(err.Error(), "schema_version") {
		t.Fatalf("DecodeJSONL error = %v, want missing schema_version rejection", err)
	}
}
