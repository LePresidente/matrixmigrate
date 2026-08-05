package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRecordsConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("language: en\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if cfg.ConfigFile == "" {
		t.Fatal("ConfigFile is empty although a config.yaml was read")
	}
}

func TestLoadWithoutConfigFileLeavesConfigFileEmpty(t *testing.T) {
	t.Chdir(t.TempDir())

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	// Defaults are a legitimate outcome; callers distinguish the two cases by this field
	// rather than being told a file was parsed when none was.
	if cfg.ConfigFile != "" {
		t.Fatalf("ConfigFile should be empty when no file exists, got %q", cfg.ConfigFile)
	}
	if cfg.Language != "en" {
		t.Fatalf("defaults not applied: Language = %q", cfg.Language)
	}
}

func TestFindOverlookedConfigFile(t *testing.T) {
	empty := t.TempDir()
	if got := findOverlookedConfigFile([]string{empty}); got != "" {
		t.Fatalf("empty directory should yield no candidate, got %q", got)
	}

	withConfig := t.TempDir()
	want := filepath.Join(withConfig, "config.yaml")
	if err := os.WriteFile(want, []byte("language: en\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := findOverlookedConfigFile([]string{withConfig}); got != want {
		t.Fatalf("findOverlookedConfigFile() = %q, want %q", got, want)
	}

	// Other extensions viper would accept are found too.
	tomlDir := t.TempDir()
	wantToml := filepath.Join(tomlDir, "config.toml")
	if err := os.WriteFile(wantToml, []byte("language = \"en\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := findOverlookedConfigFile([]string{tomlDir}); got != wantToml {
		t.Fatalf("findOverlookedConfigFile() = %q, want %q", got, wantToml)
	}
}
