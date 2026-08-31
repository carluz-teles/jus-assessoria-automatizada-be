package totp

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg" // register JPEG decoding (image.Decode auto-detects by content)
	_ "image/png"  // register PNG decoding
	"net/url"
	"strings"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
)

// ErrSecretNotFound signals input that is neither a bare base32 secret nor an
// otpauth:// URI carrying a "secret" parameter — the two shapes a lawyer's
// screenshot or copy-pasted manual key can take.
var ErrSecretNotFound = errors.New("totp: no secret found in input")

// DecodeQRImage reads a screenshot (PNG or JPEG — the two formats a phone/browser
// screenshot realistically comes in) of a TOTP enrollment QR code and returns its
// raw decoded payload (an "otpauth://totp/..." URI, per every authenticator app's
// convention) for ExtractSecret to parse. This is the one-time, human-assisted
// capture step this package exists to avoid needing ever again: once the seed is
// extracted here and sealed in the vault (internal/court), every future login
// generates its own code programmatically — no more screenshots.
func DecodeQRImage(imageBytes []byte) (string, error) {
	img, _, err := image.Decode(bytes.NewReader(imageBytes))
	if err != nil {
		return "", fmt.Errorf("totp: decode image: %w", err)
	}
	bitmap, err := gozxing.NewBinaryBitmapFromImage(img)
	if err != nil {
		return "", fmt.Errorf("totp: build bitmap: %w", err)
	}
	result, err := qrcode.NewQRCodeReader().Decode(bitmap, nil)
	if err != nil {
		return "", fmt.Errorf("totp: no QR code found in image: %w", err)
	}
	return result.GetText(), nil
}

// ExtractSecret pulls the base32 TOTP secret out of input, which may be either:
//   - a full "otpauth://totp/Issuer:account?secret=XXXX&issuer=Issuer&..." URI —
//     what DecodeQRImage returns, and what a lawyer might copy-paste from an
//     authenticator app's export; the "secret" query parameter is extracted.
//   - a bare manual-entry key — what Keycloak's own "Unable to scan?" toggle
//     shows as plain text, no image/QR involved at all.
//
// Either way, the result is handed to GenerateCode as-is (which already tolerates
// spacing/case/padding — see decodeSecret) — ExtractSecret does NOT validate that
// the string is valid base32; that happens (loudly) the first time a code is
// generated from it.
func ExtractSecret(input string) (string, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", ErrSecretNotFound
	}
	if !strings.HasPrefix(strings.ToLower(trimmed), "otpauth://") {
		return trimmed, nil
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("totp: parse otpauth URI: %w", err)
	}
	secret := u.Query().Get("secret")
	if secret == "" {
		return "", ErrSecretNotFound
	}
	return secret, nil
}

// ExtractAccounts is the uniform entry point over every input shape this
// package accepts: a Google Authenticator "Export accounts" migration QR
// (otpauth-migration://, possibly bundling several accounts — see
// DecodeMigrationURI's doc for why this exists: it avoids ANY reset of the
// lawyer's existing eproc 2FA, unlike capturing eproc's own reconfiguration
// QR), a standard otpauth:// QR, or a bare manual-entry key. The non-migration
// cases delegate to ExtractSecret (unchanged) and wrap its single result as a
// one-element list, so a caller can treat every shape uniformly — most of the
// time there is exactly one Account and no disambiguation is needed.
func ExtractAccounts(input string) ([]Account, error) {
	trimmed := strings.TrimSpace(input)
	if strings.HasPrefix(strings.ToLower(trimmed), "otpauth-migration://") {
		return DecodeMigrationURI(trimmed)
	}
	secret, err := ExtractSecret(trimmed)
	if err != nil {
		return nil, err
	}
	return []Account{{Secret: secret}}, nil
}
