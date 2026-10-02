package config

import (
	"strings"
	"testing"
)

func TestValidateHostKeyFingerprint(t *testing.T) {
	const good = "SHA256:Q2xhdWRlRXhhbXBsZUZpbmdlcnByaW50MDAwMDAwMA"
	for _, side := range []string{"mattermost", "matrix"} {
		withFingerprint := func(fp string) *Config {
			c := &Config{}
			ssh := &c.Mattermost.SSH
			if side == "matrix" {
				ssh = &c.Matrix.SSH
			}
			ssh.HostKeyFingerprint = fp
			return c
		}
		if err := withFingerprint("").Validate(); err != nil {
			t.Errorf("%s: unset fingerprint rejected: %v", side, err)
		}
		if err := withFingerprint(good).Validate(); err != nil {
			t.Errorf("%s: valid fingerprint rejected: %v", side, err)
		}
		for _, bad := range []string{
			"256 " + good + " root@example.com (ED25519)", // the whole ssh-keygen -lf line
			"Q2xhdWRlRXhhbXBsZUZpbmdlcnByaW50MDAwMDAwMA",  // no SHA256: prefix
			"MD5:16:27:ac:a5:76:28:2d:36:63:1b:56:4d:eb:df:a6:48",
			good + " ",
		} {
			err := withFingerprint(bad).Validate()
			if err == nil {
				t.Errorf("%s: %q accepted", side, bad)
				continue
			}
			for _, want := range []string{side + ".ssh.host_key_fingerprint", "SHA256:"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s: error %q does not mention %q", side, err, want)
				}
			}
		}
	}
}
