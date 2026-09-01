package totp

import (
	"encoding/base32"
	"testing"
	"time"
)

// TestHOTP_RFC4226Vectors pins hotp() against RFC 4226 Appendix D's official test
// values — secret "12345678901234567890" (ASCII), counters 0..9, 6-digit truncation.
// These are the canonical vectors every HOTP/TOTP implementation is checked against.
func TestHOTP_RFC4226Vectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	tests := []struct {
		counter uint64
		want    string
	}{
		{0, "755224"},
		{1, "287082"},
		{2, "359152"},
		{3, "969429"},
		{4, "338314"},
		{5, "254676"},
		{6, "287922"},
		{7, "162583"},
		{8, "399871"},
		{9, "520489"},
	}
	for _, tt := range tests {
		if got := hotp(secret, tt.counter); got != tt.want {
			t.Errorf("hotp(secret, %d) = %q, want %q", tt.counter, got, tt.want)
		}
	}
}

// TestGenerateCode_MatchesHOTPAtCounter proves the time-to-counter step math: a Unix
// time inside step*N's window (30s) must produce the same code as hotp(secret, N).
func TestGenerateCode_MatchesHOTPAtCounter(t *testing.T) {
	secret := base32Encode([]byte("12345678901234567890"))

	tests := []struct {
		unixSeconds int64
		wantCounter uint64
	}{
		{0, 0},
		{29, 0},
		{30, 1},
		{59, 1},
		{60, 2},
	}
	for _, tt := range tests {
		got, err := GenerateCode(secret, time.Unix(tt.unixSeconds, 0).UTC())
		if err != nil {
			t.Fatalf("GenerateCode(%d): %v", tt.unixSeconds, err)
		}
		want := hotp([]byte("12345678901234567890"), tt.wantCounter)
		if got != want {
			t.Errorf("GenerateCode at unix=%d = %q, want %q (counter %d)", tt.unixSeconds, got, want, tt.wantCounter)
		}
	}
}

// TestGenerateCode_AcceptsRealWorldSecretRenderings proves decodeSecret tolerates the
// shapes a real enrollment page or authenticator app export actually produces:
// lower-case, spaced-out groups (Keycloak's manual key display), and '=' padding.
func TestGenerateCode_AcceptsRealWorldSecretRenderings(t *testing.T) {
	canonical := base32Encode([]byte("12345678901234567890"))
	at := time.Unix(30, 0).UTC()

	want, err := GenerateCode(canonical, at)
	if err != nil {
		t.Fatalf("GenerateCode(canonical): %v", err)
	}

	variants := map[string]string{
		"lowercase":     toLower(canonical),
		"spaced groups": spaceOutInFours(canonical),
		"padded":        canonical + "====", // harmless: TrimRight strips it back off
	}
	for name, variant := range variants {
		got, err := GenerateCode(variant, at)
		if err != nil {
			t.Fatalf("%s: GenerateCode: %v", name, err)
		}
		if got != want {
			t.Errorf("%s: GenerateCode = %q, want %q (same as canonical)", name, got, want)
		}
	}
}

func TestGenerateCode_InvalidSecretIsRejected(t *testing.T) {
	_, err := GenerateCode("not-valid-base32!!!", time.Now())
	if err == nil {
		t.Fatal("GenerateCode with invalid base32 = nil error, want an error")
	}
}

// --- test helpers ---

func base32Encode(b []byte) string {
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
}

func toLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

func spaceOutInFours(s string) string {
	var sb []byte
	for i, c := range []byte(s) {
		if i > 0 && i%4 == 0 {
			sb = append(sb, ' ')
		}
		sb = append(sb, c)
	}
	return string(sb)
}
