package matrix

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// closedRoomServer refuses the admin's own join, but accepts an invite sent by a member.
func closedRoomServer(t *testing.T, reason string) (*httptest.Server, *bool) {
	t.Helper()
	invited := false
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.Contains(path, "/account/whoami"):
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"user_id": "@admin:example.com"})

		case strings.HasSuffix(path, "/invite"):
			invited = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))

		case strings.Contains(path, "/join"):
			// The bare join is refused until an invite has been issued.
			if !invited {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"errcode":"M_FORBIDDEN","error":"` + reason + `"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"room_id":"!closed:example.com"}`))

		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &invited
}

func TestEnsureAdminCanActInRecoversInviteOnlyRoom(t *testing.T) {
	srv, invited := closedRoomServer(t, "You are not invited to this room.")
	c := NewClient(srv.URL, "admin-token", "example.com")
	c.SetASToken("as-token")
	i := NewImporter(c)

	err := i.ensureAdminCanActIn("!closed", []string{"@member:example.com"})
	if err != nil {
		t.Fatalf("expected recovery via member invite, got %v", err)
	}
	if !*invited {
		t.Fatal("an existing member should have been asked to invite the admin")
	}
}

func TestEnsureAdminCanActInRecoversRestrictedRoom(t *testing.T) {
	// The space-restricted phrasing differs from invite-only and must match too.
	srv, invited := closedRoomServer(t, "You do not belong to any of the required rooms/spaces to join this room.")
	c := NewClient(srv.URL, "admin-token", "example.com")
	c.SetASToken("as-token")
	i := NewImporter(c)

	if err := i.ensureAdminCanActIn("!restricted", []string{"@member:example.com"}); err != nil {
		t.Fatalf("expected recovery for a restricted room, got %v", err)
	}
	if !*invited {
		t.Fatal("expected an invite to be sent")
	}
}

func TestEnsureAdminCanActInGivesUpWithNoMembers(t *testing.T) {
	srv, _ := closedRoomServer(t, "You are not invited to this room.")
	c := NewClient(srv.URL, "admin-token", "example.com")
	c.SetASToken("as-token")
	i := NewImporter(c)

	if err := i.ensureAdminCanActIn("!closed", nil); err == nil {
		t.Fatal("expected an error when nobody is in the room to invite the admin")
	}
}

func TestIsRoomClosedErrIgnoresUnrelatedForbidden(t *testing.T) {
	// A permission refusal an invite would not fix must not trigger the recovery path.
	if isRoomClosedErr(errString("API error (403): M_FORBIDDEN - You don't have permission to post")) {
		t.Fatal("unrelated M_FORBIDDEN must not be treated as a closed room")
	}
	if !isRoomClosedErr(errString("API error (403): M_FORBIDDEN - You are not invited to this room.")) {
		t.Fatal("invite-only refusal should be recognised")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
