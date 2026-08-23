package config

import "testing"

func TestImportPinnedMessagesDefaultsToTrue(t *testing.T) {
	// The default lives in viper, not in the Go zero value, which for a bool is false. Losing
	// that SetDefault line would stop pins being imported with nothing failing or warning.
	cfg := loadWithConfig(t, "language: en\n")
	if !cfg.Matrix.Import.ImportPinnedMessages {
		t.Fatal("import_pinned_messages should default to true when the config file does not mention it")
	}
}

func TestImportPinnedMessagesDefaultsToTrueWithoutConfigFile(t *testing.T) {
	t.Chdir(t.TempDir())

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}
	if !cfg.Matrix.Import.ImportPinnedMessages {
		t.Fatal("import_pinned_messages should default to true when there is no config file at all")
	}
}

func TestImportPinnedMessagesCanBeDisabled(t *testing.T) {
	cfg := loadWithConfig(t, "matrix:\n  import:\n    import_pinned_messages: false\n")
	if cfg.Matrix.Import.ImportPinnedMessages {
		t.Fatal("import_pinned_messages: false in the config file was not honoured")
	}
}
