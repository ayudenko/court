package main

import (
	"os"
	"path/filepath"
	"testing"

	"court/internal/golden"
)

func TestRunReconcilesJSONLArtifactsWithGeneratedManifest(t *testing.T) {
	outputDir := t.TempDir()
	obsolete := filepath.Join(outputDir, "obsolete_v1.jsonl")
	if err := os.WriteFile(obsolete, []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("write obsolete fixture: %v", err)
	}
	unrelated := filepath.Join(outputDir, "manual_invalid_schema.jsonl")
	if err := os.WriteFile(unrelated, []byte("manual\n"), 0o644); err != nil {
		t.Fatalf("write unrelated JSONL: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outputDir, manifestName), []byte("obsolete_v1.jsonl\n"), 0o644); err != nil {
		t.Fatalf("write ownership manifest: %v", err)
	}

	if err := run(outputDir); err != nil {
		t.Fatalf("run: %v", err)
	}
	artifacts, err := golden.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	want := make(map[string]struct{}, len(artifacts))
	for _, artifact := range artifacts {
		want[artifact.Name] = struct{}{}
	}
	manifest, err := readManifest(outputDir)
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	if len(manifest) != len(want) {
		t.Fatalf("manifest count = %d, want %d: %v", len(manifest), len(want), manifest)
	}
	for name := range want {
		if _, ok := manifest[name]; !ok {
			t.Errorf("generated artifact %q is missing from manifest", name)
		}
		if _, err := os.Stat(filepath.Join(outputDir, name)); err != nil {
			t.Errorf("generated artifact %q is missing: %v", name, err)
		}
	}
	if _, err := os.Stat(obsolete); !os.IsNotExist(err) {
		t.Fatalf("obsolete trace still exists: %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("unrelated JSONL file was removed: %v", err)
	}
}
