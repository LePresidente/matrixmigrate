package migration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrCreateMessageMapping(t *testing.T) {
	const hs = "https://matrix.example.com"

	t.Run("no file gives fresh mapping", func(t *testing.T) {
		m, err := loadOrCreateMessageMapping("", hs)
		if err != nil || m == nil || m.Count() != 0 {
			t.Fatalf("got %v, %v", m, err)
		}
	})

	t.Run("valid file is loaded", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "message-mapping-1.json")
		src := NewMessageMapping(hs)
		src.AddMessage(&MessageMapEntry{MattermostID: "p1", MatrixEventID: "$e1", RoomID: "!r:example.com"})
		if err := SaveMessageMapping(src, path); err != nil {
			t.Fatal(err)
		}
		m, err := loadOrCreateMessageMapping(path, hs)
		if err != nil {
			t.Fatal(err)
		}
		if m.Count() != 1 || !m.HasMessage("p1") {
			t.Errorf("mapping not loaded: count=%d", m.Count())
		}
	})

	t.Run("corrupt file is an error naming the file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "message-mapping-2.json")
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		m, err := loadOrCreateMessageMapping(path, hs)
		if err == nil || m != nil {
			t.Fatalf("expected error, got %v, %v", m, err)
		}
		if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "NOT started") {
			t.Errorf("error should name file and say NOT started: %v", err)
		}
	})
}

func TestSaveMessageMappingMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "message-mapping-3.json")
	if err := SaveMessageMapping(NewMessageMapping("https://matrix.example.com"), path); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
}
