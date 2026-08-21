package mattermost

import (
	"strings"
	"testing"
)

// postColumns must keep the same column order in both variants: GetPosts scans positionally,
// so a divergence would silently load the wrong value into the wrong field.
func TestPostColumnsSelectsPinnedFlagWhenPresent(t *testing.T) {
	got := postColumns(true)
	if !strings.HasSuffix(got, "COALESCE(ispinned, false) AS ispinned") {
		t.Fatalf("postColumns(true) should end with the ispinned column, got:\n%s", got)
	}
}

func TestPostColumnsFallsBackToFalseWhenColumnMissing(t *testing.T) {
	got := postColumns(false)
	if !strings.HasSuffix(got, "false AS ispinned") {
		t.Fatalf("postColumns(false) should end with a literal false, got:\n%s", got)
	}
	if strings.Contains(got, "COALESCE(ispinned") {
		t.Fatalf("postColumns(false) must not reference the missing column, got:\n%s", got)
	}
}

func TestPostColumnsShareTheSamePrefix(t *testing.T) {
	withPin := strings.TrimSuffix(postColumns(true), "COALESCE(ispinned, false) AS ispinned")
	withoutPin := strings.TrimSuffix(postColumns(false), "false AS ispinned")
	if withPin != withoutPin {
		t.Fatalf("column order differs between variants:\n%q\nvs\n%q", withPin, withoutPin)
	}
}
