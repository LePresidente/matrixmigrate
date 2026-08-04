package matrix

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
)

// PasswordMode controls whether imported users get a generated password.
type PasswordMode string

const (
	// PasswordModeRandom generates a distinct random password per user.
	PasswordModeRandom PasswordMode = "random"
	// PasswordModeNone creates users without a password, so they can only authenticate
	// through SSO/MAS or an admin-initiated reset.
	PasswordModeNone PasswordMode = "none"
	// PasswordModeLocalOnly generates a password only for users who had no SSO provider in
	// Mattermost. Everyone else signs in through the upstream provider, where a local
	// password would go unused and only widen the attack surface.
	PasswordModeLocalOnly PasswordMode = "local_only"
)

// Password length bounds. The minimum is well above what a shared-secret migration needs;
// the maximum keeps generated values inside Synapse's request limits.
const (
	MinPasswordLength     = 12
	MaxPasswordLength     = 128
	DefaultPasswordLength = 24
)

// passwordCharset deliberately omits visually ambiguous characters (0/O, 1/l/I) and any
// character that would need escaping in the generated CSV or in a shell command.
const passwordCharset = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789!#%*+-=?@_"

// PasswordPolicy decides what password, if any, a newly created user is given.
// The zero value is not usable — construct with DefaultPasswordPolicy.
type PasswordPolicy struct {
	Mode   PasswordMode
	Length int
}

// DefaultPasswordPolicy returns the safe default: a distinct random password per user.
func DefaultPasswordPolicy() PasswordPolicy {
	return PasswordPolicy{Mode: PasswordModeRandom, Length: DefaultPasswordLength}
}

// Generate returns the password for one user. It returns an empty string (and no error)
// when the policy is PasswordModeNone, so callers can pass the result straight through to
// CreateUserRequest.Password, where empty means "do not set a password".
//
// PasswordModeLocalOnly needs to know which user it is deciding for, which Generate cannot
// know; it therefore assumes a local account and generates. That errs toward a usable
// account rather than an unreachable one. Callers with a user in hand should use GenerateFor.
func (p PasswordPolicy) Generate() (string, error) {
	return p.GenerateFor("")
}

// GenerateFor returns the password for one user, given the Mattermost auth_service they
// were authenticated against ("" for a local account, "gitlab"/"ldap"/... for SSO).
//
// Only PasswordModeLocalOnly looks at authService; the other modes ignore it, so callers
// can use this unconditionally.
func (p PasswordPolicy) GenerateFor(authService string) (string, error) {
	switch p.Mode {
	case PasswordModeNone:
		return "", nil
	case PasswordModeLocalOnly:
		if strings.TrimSpace(authService) != "" {
			return "", nil
		}
	}
	length := p.Length
	if length == 0 {
		length = DefaultPasswordLength
	}
	return GenerateRandomPassword(length)
}

// GenerateRandomPassword returns a cryptographically random password of the given length,
// drawn uniformly from passwordCharset.
//
// crypto/rand.Int is used rather than reducing raw bytes modulo the charset size, which
// would bias the result toward the first characters of the set.
func GenerateRandomPassword(length int) (string, error) {
	if length < MinPasswordLength || length > MaxPasswordLength {
		return "", fmt.Errorf("password length %d out of range (%d-%d)", length, MinPasswordLength, MaxPasswordLength)
	}

	max := big.NewInt(int64(len(passwordCharset)))
	out := make([]byte, length)
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("failed to read secure random bytes: %w", err)
		}
		out[i] = passwordCharset[n.Int64()]
	}
	return string(out), nil
}

// UserCredential records a generated password so the operator can distribute it.
// Only produced when the policy generates passwords.
type UserCredential struct {
	Username     string
	MatrixUserID string
	Password     string
}
