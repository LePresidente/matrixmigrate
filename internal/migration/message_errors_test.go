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

func TestCategorizeMessageErrorsCountsPinFailures(t *testing.T) {
	// Pin failures have their own remedy - usually a missing Application Service token in a
	// room the admin does not own - so they must not disappear into "other".
	counts := CategorizeMessageErrors([]string{
		"Failed to pin messages in room !room:example.com: API error (403): M_FORBIDDEN - no power",
		"Failed to read pinned messages of room !room:example.com: API error (403): M_FORBIDDEN - not in room",
		"No room mapping for channel c1 (post p1)",
	})
	if counts["pin_error"] != 2 {
		t.Fatalf("pin_error = %d, want 2 (counts: %v)", counts["pin_error"], counts)
	}
	if counts["other"] != 0 {
		t.Fatalf("pin failures must not land in other (counts: %v)", counts)
	}
}
