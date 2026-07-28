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
