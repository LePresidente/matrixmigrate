package matrix

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestEnsureFallbackSenderJoinsOncePerRoom(t *testing.T) {
	var mu sync.Mutex
	joins := []string{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/account/whoami"):
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"user_id": "@matrixmigrate:example.com"})
		case strings.Contains(r.URL.Path, "/_synapse/admin/v1/join/"):
			var body struct {
				UserID string `json:"user_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			joins = append(joins, body.UserID)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"room_id":"!r:example.com"}`))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(srv.URL, "admin-token", "example.com")
	c.SetASToken("as-token")
	i := NewImporter(c)

	for n := 0; n < 3; n++ {
		if err := i.ensureFallbackSenderInRoom("!room"); err != nil {
			t.Fatalf("ensureFallbackSenderInRoom: %v", err)
		}
	}

	botJoins := 0
	for _, j := range joins {
		if j == "@matrixmigrate:example.com" {
			botJoins++
		}
	}
	if botJoins != 1 {
		t.Fatalf("bot should be joined once per room, got %d joins: %v", botJoins, joins)
	}
	// Recorded so the withdrawal pass takes the bot back out again.
	if len(i.HistoryJoins()) != 1 || i.HistoryJoins()[0].UserID != "@matrixmigrate:example.com" {
		t.Fatalf("bot join should be recorded for cleanup, got %#v", i.HistoryJoins())
	}
}

func TestEnsureFallbackSenderWithoutASTokenErrors(t *testing.T) {
	c := NewClient("http://127.0.0.1:1", "admin-token", "example.com")
	i := NewImporter(c)
	if err := i.ensureFallbackSenderInRoom("!room"); err == nil {
		t.Fatal("expected an error when there is no AS token to resolve the fallback sender")
	}
}
