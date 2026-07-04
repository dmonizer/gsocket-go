package gsocket

import (
	"encoding/hex"
	"testing"
)

func TestDeriveKeyMaterial(t *testing.T) {
	tests := []struct {
		name       string
		secret     string
		wantAddrLen int
	}{
		{
			name:        "simple secret",
			secret:      "MySecret",
			wantAddrLen: AddrSize,
		},
		{
			name:        "empty secret",
			secret:      "",
			wantAddrLen: AddrSize,
		},
		{
			name:        "long secret",
			secret:      "this is a very long secret that exceeds typical password length",
			wantAddrLen: AddrSize,
		},
		{
			name:        "unicode secret",
			secret:      "café💻🔐",
			wantAddrLen: AddrSize,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srpPassword, addr := DeriveKeyMaterial(tt.secret)

			// SRP password should be 64 hex chars (SHA256 = 32 bytes).
			if len(srpPassword) != 64 {
				t.Errorf("srpPassword length = %d, want 64", len(srpPassword))
			}

			// Verify it's valid hex.
			if _, err := hex.DecodeString(srpPassword); err != nil {
				t.Errorf("srpPassword is not valid hex: %v", err)
			}

			// Address should be 16 bytes.
			if len(addr) != tt.wantAddrLen {
				t.Errorf("addr length = %d, want %d", len(addr), tt.wantAddrLen)
			}
		})
	}
}

func TestDeriveKeyMaterialDeterministic(t *testing.T) {
	// Same secret should always produce the same result.
	secret := "test-secret-12345"

	pwd1, addr1 := DeriveKeyMaterial(secret)
	pwd2, addr2 := DeriveKeyMaterial(secret)

	if pwd1 != pwd2 {
		t.Error("srpPassword not deterministic")
	}
	if addr1 != addr2 {
		t.Error("addr not deterministic")
	}
}

func TestDeriveKeyMaterialDifferentSecrets(t *testing.T) {
	// Different secrets should produce different results.
	pwd1, addr1 := DeriveKeyMaterial("secret-one")
	pwd2, addr2 := DeriveKeyMaterial("secret-two")

	if pwd1 == pwd2 {
		t.Error("different secrets produced same srpPassword")
	}
	if addr1 == addr2 {
		t.Error("different secrets produced same addr")
	}
}

func TestAddrHostnameID(t *testing.T) {
	// Hostname ID should be 0-25.
	for _, secret := range []string{"a", "b", "test", "MySecret", "another-secret"} {
		_, addr := DeriveKeyMaterial(secret)
		id := addr.HostnameID()
		if id > 25 {
			t.Errorf("HostnameID() = %d for secret %q, want 0-25", id, secret)
		}
	}
}

func TestAddrGSRNHostname(t *testing.T) {
	_, addr := DeriveKeyMaterial("test")
	hostname := addr.GSRNHostname()

	if len(hostname) == 0 {
		t.Error("GSRNHostname() returned empty string")
	}

	// Should match pattern: <letter>.gs.thc.org
	if hostname[len(hostname)-11:] != ".gs.thc.org" {
		t.Errorf("GSRNHostname() = %q, want ending with .gs.thc.org", hostname)
	}
}

func TestAddrString(t *testing.T) {
	_, addr := DeriveKeyMaterial("test")
	s := addr.String()

	if len(s) != 32 {
		t.Errorf("Addr.String() length = %d, want 32", len(s))
	}

	// Should be valid hex.
	if _, err := hex.DecodeString(s); err != nil {
		t.Errorf("Addr.String() is not valid hex: %v", err)
	}
}

func TestDeriveToken(t *testing.T) {
	_, addr := DeriveKeyMaterial("test")

	// Empty auth should give zero token.
	zeroToken := DeriveToken("", addr)
	for i, b := range zeroToken {
		if b != 0 {
			t.Errorf("empty auth token[%d] = %d, want 0", i, b)
		}
	}

	// Non-empty auth should give non-zero token.
	authToken := DeriveToken("authstring", addr)
	allZero := true
	for _, b := range authToken {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Error("auth token is all zeros for non-empty auth string")
	}

	// Same inputs should give same token.
	token1 := DeriveToken("auth", addr)
	token2 := DeriveToken("auth", addr)
	if token1 != token2 {
		t.Error("DeriveToken is not deterministic")
	}
}
