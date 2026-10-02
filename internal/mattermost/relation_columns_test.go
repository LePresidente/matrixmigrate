package mattermost

import "testing"

// The column probe itself needs a database; these cover what each table makes of its answer.

func TestPostsSchemaFromColumnsSeesThePinnedFlag(t *testing.T) {
	schema, err := postsSchemaFromColumns(map[string]bool{"id": true, "ispinned": true}, true)
	if err != nil {
		t.Fatalf("postsSchemaFromColumns returned %v", err)
	}
	if !schema.hasIsPinned {
		t.Fatal("hasIsPinned = false for a posts table with an ispinned column")
	}

	schema, err = postsSchemaFromColumns(map[string]bool{"id": true}, true)
	if err != nil || schema.hasIsPinned {
		t.Fatalf("postsSchemaFromColumns = %+v, %v; want no pinned flag and no error", schema, err)
	}
}

func TestPostsSchemaFromColumnsRejectsAnUnresolvedTable(t *testing.T) {
	// Without a posts table there is nothing to export; carrying on would only fail later
	// with a less useful error, or worse, export nothing and call it success.
	if _, err := postsSchemaFromColumns(nil, false); err == nil {
		t.Fatal("postsSchemaFromColumns accepted a posts table that does not resolve")
	}
}

func TestReactionsSchemaFromColumns(t *testing.T) {
	if got := reactionsSchemaFromColumns(nil, false); got.exists {
		t.Fatalf("reactionsSchemaFromColumns = %+v for an unresolved table, want exists=false", got)
	}
	got := reactionsSchemaFromColumns(map[string]bool{"postid": true, "deleteat": true}, true)
	if !got.exists || !got.hasDeleteAt {
		t.Fatalf("reactionsSchemaFromColumns = %+v, want exists and hasDeleteAt", got)
	}
	got = reactionsSchemaFromColumns(map[string]bool{"postid": true}, true)
	if !got.exists || got.hasDeleteAt {
		t.Fatalf("reactionsSchemaFromColumns = %+v, want exists without hasDeleteAt", got)
	}
}
