package config

import (
	"os"
	"path/filepath"
	"testing"
)

// loadWithConfig writes body to a config.yaml in a temporary working directory and loads it.
func loadWithConfig(t *testing.T, body string) *Config {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	return cfg
}

func TestImportReactionsDefaultsToTrue(t *testing.T) {
	// The default lives in viper, not in the Go zero value, which for a bool is false. If that
	// SetDefault line is ever lost, reactions stop being imported without anything failing or
	// warning - so it gets its own test rather than being assumed.
	cfg := loadWithConfig(t, "language: en\n")
	if !cfg.Matrix.Import.ImportReactions {
		t.Fatal("import_reactions should default to true when the config file does not mention it")
	}
}

func TestImportReactionsDefaultsToTrueWithoutConfigFile(t *testing.T) {
	t.Chdir(t.TempDir())

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if !cfg.Matrix.Import.ImportReactions {
		t.Fatal("import_reactions should default to true when there is no config file at all")
	}
}

func TestImportReactionsCanBeDisabled(t *testing.T) {
	cfg := loadWithConfig(t, "matrix:\n  import:\n    import_reactions: false\n")
	if cfg.Matrix.Import.ImportReactions {
		t.Fatal("import_reactions: false in the config file was not honoured")
	}
}

func TestImportReactionsExplicitTrue(t *testing.T) {
	cfg := loadWithConfig(t, "matrix:\n  import:\n    import_reactions: true\n")
	if !cfg.Matrix.Import.ImportReactions {
		t.Fatal("import_reactions: true in the config file was not honoured")
	}
}
