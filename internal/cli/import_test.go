package cli

import (
	"testing"

	"github.com/aligundogdu/matrixmigrate/internal/matrix"
)

func TestProgressStageForReactions(t *testing.T) {
	label, unit := progressStageFor(matrix.ReactionProgressStage)
	if label != "Reactions" || unit != "reactions" {
		t.Fatalf("progressStageFor(reactions) = (%q, %q), want (Reactions, reactions)", label, unit)
	}
}

func TestProgressStageForPins(t *testing.T) {
	// The pin pass counts rooms, not messages, so the unit noun must say "rooms" - this is
	// what regressed before the fix: the pin stage fell through to the zero value and the CLI
	// kept printing whatever label/unit the previous pass (reactions or messages) had left.
	label, unit := progressStageFor(matrix.PinProgressStage)
	if label != "Pinned messages" || unit != "rooms" {
		t.Fatalf("progressStageFor(pins) = (%q, %q), want (Pinned messages, rooms)", label, unit)
	}
}

func TestProgressStageForMessagesIsEmpty(t *testing.T) {
	// An empty label tells the caller to leave its current label/unit alone - the base message
	// pass does not carry a stage constant of its own.
	label, unit := progressStageFor("")
	if label != "" || unit != "" {
		t.Fatalf("progressStageFor(\"\") = (%q, %q), want (\"\", \"\")", label, unit)
	}
}
