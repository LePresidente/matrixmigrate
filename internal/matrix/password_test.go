package matrix

import (
	"strings"
	"testing"
)

func TestGenerateRandomPasswordLengthAndCharset(t *testing.T) {
	for _, length := range []int{MinPasswordLength, DefaultPasswordLength, MaxPasswordLength} {
		pw, err := GenerateRandomPassword(length)
		if err != nil {
			t.Fatalf("GenerateRandomPassword(%d) returned error: %v", length, err)
		}
		if len(pw) != length {
			t.Fatalf("want length %d, got %d (%q)", length, len(pw), pw)
		}
		for _, r := range pw {
			if !strings.ContainsRune(passwordCharset, r) {
				t.Fatalf("password %q contains out-of-charset rune %q", pw, r)
			}
		}
	}
}

func TestGenerateRandomPasswordRejectsOutOfRangeLength(t *testing.T) {
	for _, length := range []int{0, -1, MinPasswordLength - 1, MaxPasswordLength + 1} {
		if _, err := GenerateRandomPassword(length); err == nil {
			t.Fatalf("length %d should be rejected", length)
		}
	}
}

// The whole point of the change: two users must never share a password.
func TestGenerateRandomPasswordIsUnique(t *testing.T) {
	const runs = 200
	seen := make(map[string]struct{}, runs)
	for i := 0; i < runs; i++ {
		pw, err := GenerateRandomPassword(DefaultPasswordLength)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, dup := seen[pw]; dup {
			t.Fatalf("duplicate password generated after %d runs: %q", i, pw)
		}
		seen[pw] = struct{}{}
	}
}

func TestPasswordPolicyNoneReturnsEmpty(t *testing.T) {
	pw, err := PasswordPolicy{Mode: PasswordModeNone, Length: DefaultPasswordLength}.Generate()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pw != "" {
		t.Fatalf("PasswordModeNone must produce an empty password, got %q", pw)
	}
}

func TestPasswordPolicyDefaultsLengthWhenUnset(t *testing.T) {
	pw, err := PasswordPolicy{Mode: PasswordModeRandom}.Generate()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pw) != DefaultPasswordLength {
		t.Fatalf("want default length %d, got %d", DefaultPasswordLength, len(pw))
	}
}

func TestDefaultPasswordPolicyGeneratesRandom(t *testing.T) {
	p := DefaultPasswordPolicy()
	if p.Mode != PasswordModeRandom {
		t.Fatalf("default policy must generate passwords, got mode %q", p.Mode)
	}
	pw, err := p.Generate()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pw == "" {
		t.Fatal("default policy produced an empty password")
	}
}

func TestPasswordPolicyGenerateFor(t *testing.T) {
	tests := []struct {
		name        string
		mode        PasswordMode
		authService string
		wantPass    bool
	}{
		{"random ignores auth_service for local account", PasswordModeRandom, "", true},
		{"random ignores auth_service for SSO account", PasswordModeRandom, "gitlab", true},
		{"none never generates", PasswordModeNone, "", false},
		{"local_only generates for local account", PasswordModeLocalOnly, "", true},
		{"local_only skips gitlab SSO", PasswordModeLocalOnly, "gitlab", false},
		{"local_only skips ldap SSO", PasswordModeLocalOnly, "ldap", false},
		// Mattermost stores an unset auth_service as NULL, which the query coalesces to "";
		// stray whitespace must not be mistaken for a provider name.
		{"local_only treats whitespace as local", PasswordModeLocalOnly, "   ", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := PasswordPolicy{Mode: tt.mode, Length: DefaultPasswordLength}
			pw, err := p.GenerateFor(tt.authService)
			if err != nil {
				t.Fatalf("GenerateFor(%q) returned error: %v", tt.authService, err)
			}
			if tt.wantPass && pw == "" {
				t.Fatalf("mode %q, auth_service %q: expected a password, got none", tt.mode, tt.authService)
			}
			if !tt.wantPass && pw != "" {
				t.Fatalf("mode %q, auth_service %q: expected no password, got %q", tt.mode, tt.authService, pw)
			}
		})
	}
}

func TestPasswordPolicyGenerateLocalOnlyWithoutUser(t *testing.T) {
	// Generate() has no user context, so local_only must not hand out a password by
	// default — callers that care use GenerateFor.
	p := PasswordPolicy{Mode: PasswordModeLocalOnly, Length: DefaultPasswordLength}
	pw, err := p.Generate()
	if err != nil {
		t.Fatalf("Generate() returned error: %v", err)
	}
	if pw == "" {
		t.Fatal("Generate() with local_only should behave as an unknown (local) user and generate")
	}
}
