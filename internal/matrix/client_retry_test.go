package matrix

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsNonIdempotent(t *testing.T) {
	tests := []struct {
		name     string
		method   string
		endpoint string
		want     bool
	}{
		{"createRoom as admin", "POST", "/_matrix/client/v3/createRoom", true},
		{"createRoom as user via AS", "POST", "/_matrix/client/v3/createRoom?user_id=%40alice%3Aexample.com", true},
		{"sending a message is keyed by txn id", "PUT", "/_matrix/client/v3/rooms/!r%3Aexample.com/send/m.room.message/txn1", false},
		{"reading the room directory", "GET", "/_matrix/client/v3/directory/room/%23general%3Aexample.com", false},
		{"inviting is safe to replay", "POST", "/_matrix/client/v3/rooms/!r%3Aexample.com/invite", false},
		{"createRoom under another method", "GET", "/_matrix/client/v3/createRoom", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNonIdempotent(tt.method, tt.endpoint); got != tt.want {
				t.Errorf("isNonIdempotent(%q, %q) = %v, want %v", tt.method, tt.endpoint, got, tt.want)
			}
		})
	}
}

func TestRoomMayExist(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"no error", nil, false},
		{"create timed out", fmt.Errorf("%w: context deadline exceeded", ErrCreateRoomTimeout), true},
		{"alias already taken", errors.New("API error (400): M_ROOM_IN_USE - Room alias already taken"), true},
		{"alias in an exclusive namespace", errors.New("API error (400): M_EXCLUSIVE - room alias is reserved"), false},
		{"token rejected", errors.New("API error: M_UNKNOWN_TOKEN - Token is not active"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := roomMayExist(tt.err); got != tt.want {
				t.Errorf("roomMayExist(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// A wrapped ErrCreateRoomTimeout must stay recognisable to CreateDirectRoom, which branches on
// errors.Is to decide between looking the DM up and creating it again.
func TestCreateRoomTimeoutUnwraps(t *testing.T) {
	err := fmt.Errorf("create DM room: %w", fmt.Errorf("%w: EOF", ErrCreateRoomTimeout))
	if !errors.Is(err, ErrCreateRoomTimeout) {
		t.Errorf("errors.Is(%v, ErrCreateRoomTimeout) = false, want true", err)
	}
}
