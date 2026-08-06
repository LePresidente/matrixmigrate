package matrix

import "testing"

func TestSkipTally(t *testing.T) {
	var empty skipTally
	if got := empty.String(); got != "" {
		t.Fatalf("empty tally should render as empty string, got %q", got)
	}

	var tally skipTally
	tally.add("deleted")
	tally.add("user-not-mapped")
	tally.add("deleted")
	tally.add("deleted")

	// First-seen order, not alphabetical: the sequence tells the operator what the run hit
	// first, and a stable order keeps log lines comparable between runs.
	if got, want := tally.String(), "deleted=3, user-not-mapped=1"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestSkipTallySingleReason(t *testing.T) {
	var tally skipTally
	tally.add("both-participants-locked")
	if got, want := tally.String(), "both-participants-locked=1"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
