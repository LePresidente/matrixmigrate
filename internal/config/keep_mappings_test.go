package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeepMappingsDefaultsToKeepingEverything(t *testing.T) {
	t.Chdir(t.TempDir())

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	// Deleting files is opt-in: an upgrade must not start removing an operator's mappings.
	if cfg.Data.KeepMappings != 0 {
		t.Fatalf("KeepMappings = %d, want 0 when not configured", cfg.Data.KeepMappings)
	}
}

func TestKeepMappingsIsReadFromConfig(t *testing.T) {
	dir := t.TempDir()
	content := "data:\n  keep_mappings: 14\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if cfg.Data.KeepMappings != 14 {
		t.Fatalf("KeepMappings = %d, want 14", cfg.Data.KeepMappings)
	}
}

func TestValidateRejectsNegativeKeepMappings(t *testing.T) {
	cfg := &Config{}
	cfg.Data.KeepMappings = -1

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() accepted data.keep_mappings: -1")
	}
	if !strings.Contains(err.Error(), "data.keep_mappings") {
		t.Fatalf("error does not name the option: %v", err)
	}
}
