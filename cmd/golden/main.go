package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"court/internal/golden"
)

const manifestName = ".golden-manifest"

func main() {
	out := flag.String("out", "internal/golden/testdata", "directory for generated JSONL traces")
	flag.Parse()
	if err := run(*out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(outputDir string) error {
	artifacts, err := golden.Generate()
	if err != nil {
		return fmt.Errorf("generate golden traces: %w", err)
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	previous, err := readManifest(outputDir)
	if err != nil {
		return err
	}
	expected := make(map[string]struct{}, len(artifacts))
	for _, artifact := range artifacts {
		if filepath.Base(artifact.Name) != artifact.Name {
			return fmt.Errorf("invalid artifact name %q", artifact.Name)
		}
		if _, duplicate := expected[artifact.Name]; duplicate {
			return fmt.Errorf("duplicate artifact name %q", artifact.Name)
		}
		expected[artifact.Name] = struct{}{}
		path := filepath.Join(outputDir, artifact.Name)
		if err := writeAtomically(path, artifact.Data); err != nil {
			return fmt.Errorf("write %s: %w", artifact.Name, err)
		}
		fmt.Println(path)
	}
	if err := removeObsoleteArtifacts(outputDir, previous, expected); err != nil {
		return err
	}
	return writeManifest(outputDir, expected)
}

func readManifest(outputDir string) (map[string]struct{}, error) {
	data, err := os.ReadFile(filepath.Join(outputDir, manifestName))
	if os.IsNotExist(err) {
		return map[string]struct{}{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read ownership manifest: %w", err)
	}
	owned := make(map[string]struct{})
	for line, name := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if name == "" {
			continue
		}
		if filepath.Base(name) != name || filepath.Ext(name) != ".jsonl" {
			return nil, fmt.Errorf("ownership manifest line %d has invalid artifact name %q", line+1, name)
		}
		if _, duplicate := owned[name]; duplicate {
			return nil, fmt.Errorf("ownership manifest contains duplicate artifact %q", name)
		}
		owned[name] = struct{}{}
	}
	return owned, nil
}

func removeObsoleteArtifacts(outputDir string, previous, expected map[string]struct{}) error {
	for name := range previous {
		if _, keep := expected[name]; keep {
			continue
		}
		path := filepath.Join(outputDir, name)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove obsolete trace %s: %w", name, err)
		}
		fmt.Println(path, "(removed obsolete trace)")
	}
	return nil
}

func writeManifest(outputDir string, expected map[string]struct{}) error {
	names := make([]string, 0, len(expected))
	for name := range expected {
		names = append(names, name)
	}
	sort.Strings(names)
	data := []byte(strings.Join(names, "\n") + "\n")
	if err := writeAtomically(filepath.Join(outputDir, manifestName), data); err != nil {
		return fmt.Errorf("write ownership manifest: %w", err)
	}
	return nil
}

func writeAtomically(path string, data []byte) (err error) {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".golden-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		if err != nil {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err = temporary.Chmod(0o644); err != nil {
		return err
	}
	if _, err = temporary.Write(data); err != nil {
		return err
	}
	if err = temporary.Sync(); err != nil {
		return err
	}
	if err = temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}
