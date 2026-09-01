// Package totp generates RFC 6238 time-based one-time codes from a seed captured once
// at MFA enrollment (see internal/court) — never from a phone. The algorithm is a fixed,
// small RFC (HOTP counter + HMAC-SHA1 dynamic truncation, RFC 4226 §5.3), so this is
// hand-rolled against stdlib crypto/hmac+crypto/sha1 instead of pulling in a dependency
// for ~40 lines of unchanging math.
package totp

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // RFC 6238 mandates SHA-1 for the default TOTP algorithm; this is HMAC keying material, not a collision target.
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// step is the RFC 6238 time step: a code is valid for this many seconds.
const step = 30 * time.Second

// digits is the code length eproc's Keycloak (and the RFC 6238 default) expects.
const digits = 6

// GenerateCode computes the 6-digit TOTP code for secretBase32 valid at instant at.
// secretBase32 is the manual entry key captured at enrollment (RFC 4648 base32,
// upper-case, padding optional — both accepted since authenticator apps and Keycloak
// itself render it either way).
func GenerateCode(secretBase32 string, at time.Time) (string, error) {
	secret, err := decodeSecret(secretBase32)
	if err != nil {
		return "", fmt.Errorf("totp: decode secret: %w", err)
	}
	counter := uint64(at.Unix()) / uint64(step.Seconds())
	return hotp(secret, counter), nil
}

// decodeSecret accepts the secret with or without base32 padding and regardless of
// case — every real-world rendering (Keycloak's manual key, an authenticator app's
// export) varies on both.
func decodeSecret(secretBase32 string) ([]byte, error) {
	clean := strings.ToUpper(strings.ReplaceAll(secretBase32, " ", ""))
	enc := base32.StdEncoding.WithPadding(base32.NoPadding)
	return enc.DecodeString(strings.TrimRight(clean, "="))
}

// hotp implements RFC 4226 §5.3 (HOTP): HMAC-SHA1 over the 8-byte big-endian counter,
// dynamic truncation, mod 10^digits, zero-padded. TOTP (RFC 6238) is exactly this with
// counter = unixTime/step instead of an incrementing counter.
func hotp(secret []byte, counter uint64) string {
	var counterBytes [8]byte
	binary.BigEndian.PutUint64(counterBytes[:], counter)

	mac := hmac.New(sha1.New, secret)
	mac.Write(counterBytes[:])
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	binCode := (uint32(sum[offset]&0x7f) << 24) |
		(uint32(sum[offset+1]) << 16) |
		(uint32(sum[offset+2]) << 8) |
		uint32(sum[offset+3])

	mod := uint32(1)
	for range digits {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, binCode%mod)
}
