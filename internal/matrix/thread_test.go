package matrix

import "testing"

func TestThreadRelation(t *testing.T) {
	rel := threadRelation("$root:example.com", "$latest:example.com")

	if rel["rel_type"] != "m.thread" {
		t.Errorf("rel_type = %v, want m.thread", rel["rel_type"])
	}
	if rel["event_id"] != "$root:example.com" {
		t.Errorf("event_id = %v, want the thread root", rel["event_id"])
	}
	if rel["is_falling_back"] != true {
		t.Errorf("is_falling_back = %v, want true", rel["is_falling_back"])
	}

	reply, ok := rel["m.in_reply_to"].(map[string]interface{})
	if !ok {
		t.Fatalf("m.in_reply_to missing or wrong type: %T", rel["m.in_reply_to"])
	}
	if reply["event_id"] != "$latest:example.com" {
		t.Errorf("fallback event_id = %v, want the latest event in the thread", reply["event_id"])
	}
}

// The first reply in a thread has no earlier event, so the fallback points at the root.
func TestThreadRelationFallsBackToRoot(t *testing.T) {
	rel := threadRelation("$root:example.com", "")

	reply, ok := rel["m.in_reply_to"].(map[string]interface{})
	if !ok {
		t.Fatalf("m.in_reply_to missing or wrong type: %T", rel["m.in_reply_to"])
	}
	if reply["event_id"] != "$root:example.com" {
		t.Errorf("fallback event_id = %v, want the root", reply["event_id"])
	}
	if rel["event_id"] != "$root:example.com" {
		t.Errorf("event_id = %v, want the root", rel["event_id"])
	}
}
