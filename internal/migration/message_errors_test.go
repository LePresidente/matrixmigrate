package migration

import (
	"os"
	"strings"
	"testing"
)

func TestWriteMessageErrors(t *testing.T) {
	dir := t.TempDir()
	errs := []string{
		"No room mapping for channel c1 (post p1)",
		"Failed to send message p2: boom",
		"Failed to send reply p3: boom",
	}

	path, err := WriteMessageErrors(dir, errs)
	if err != nil {
		t.Fatalf("WriteMessageErrors: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	for _, e := range errs {
		if !strings.Contains(string(data), e) {
			t.Fatalf("error file missing %q; got:\n%s", e, data)
		}
	}

	counts := CategorizeMessageErrors(errs)
	if counts["no_room"] != 1 || counts["send_error"] != 1 || counts["reply_error"] != 1 {
		t.Fatalf("bad categories: %v", counts)
	}
}
