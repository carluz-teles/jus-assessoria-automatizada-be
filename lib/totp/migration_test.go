package totp

import (
	"encoding/base32"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The encoder below is written INDEPENDENTLY of DecodeMigrationURI's own
// primitives (readTag/readVarint/readLenDelim) — reusing the decoder to build
// its own test fixtures would let a bug in one cancel out the same bug in the
// other. It builds real protobuf wire bytes by hand, matching the schema
// DecodeMigrationURI's doc documents.

type testOtpParam struct {
	secret    []byte
	name      string
	issuer    string
	algorithm uint64
	digits    uint64
	otpType   uint64
}

func encodeVarint(buf []byte, v uint64) []byte {
	for v >= 0x80 {
		buf = append(buf, byte(v)|0x80)
		v >>= 7
	}
	return append(buf, byte(v))
}

func encodeTag(buf []byte, fieldNum, wireType int) []byte {
	return encodeVarint(buf, uint64(fieldNum)<<3|uint64(wireType))
}

func encodeLenDelim(buf []byte, fieldNum int, payload []byte) []byte {
	buf = encodeTag(buf, fieldNum, wireLenDelim)
	buf = encodeVarint(buf, uint64(len(payload)))
	return append(buf, payload...)
}

func encodeVarintField(buf []byte, fieldNum int, v uint64) []byte {
	buf = encodeTag(buf, fieldNum, wireVarint)
	return encodeVarint(buf, v)
}

func encodeOtpParam(p testOtpParam) []byte {
	var buf []byte
	if p.secret != nil {
		buf = encodeLenDelim(buf, 1, p.secret)
	}
	if p.name != "" {
		buf = encodeLenDelim(buf, 2, []byte(p.name))
	}
	if p.issuer != "" {
		buf = encodeLenDelim(buf, 3, []byte(p.issuer))
	}
	if p.algorithm != 0 {
		buf = encodeVarintField(buf, 4, p.algorithm)
	}
	if p.digits != 0 {
		buf = encodeVarintField(buf, 5, p.digits)
	}
	if p.otpType != 0 {
		buf = encodeVarintField(buf, 6, p.otpType)
	}
	return buf
}

// encodeMigrationURI builds a real "otpauth-migration://offline?data=..." URI
// from a list of accounts — the test-side equivalent of what Google
// Authenticator's "Export accounts" feature produces.
func encodeMigrationURI(params []testOtpParam) string {
	var payload []byte
	for _, p := range params {
		payload = encodeLenDelim(payload, 1, encodeOtpParam(p))
	}
	data := base64.StdEncoding.EncodeToString(payload)
	return "otpauth-migration://offline?data=" + url.QueryEscape(data)
}

func TestDecodeMigrationURI_SingleAccount(t *testing.T) {
	t.Parallel()

	secret := []byte("12345678901234567890") // 20 bytes, a realistic TOTP secret length
	uri := encodeMigrationURI([]testOtpParam{
		{secret: secret, name: "luan.gomes", issuer: "eproc", algorithm: 1, digits: 1, otpType: 2},
	})

	accounts, err := DecodeMigrationURI(uri)
	if err != nil {
		t.Fatalf("DecodeMigrationURI: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts))
	}
	got := accounts[0]
	if got.Name != "luan.gomes" || got.Issuer != "eproc" {
		t.Errorf("name/issuer = %q/%q, want luan.gomes/eproc", got.Name, got.Issuer)
	}

	// The secret round-trips to the exact original bytes via base32.
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(got.Secret)
	if err != nil {
		t.Fatalf("decode returned secret: %v", err)
	}
	if string(decoded) != string(secret) {
		t.Errorf("secret round-trip = %q, want %q", decoded, secret)
	}

	// Sanity: GenerateCode actually accepts it (the whole point of extraction).
	if _, err := GenerateCode(got.Secret, time.Now()); err != nil {
		t.Errorf("GenerateCode on extracted secret: %v", err)
	}
}

func TestDecodeMigrationURI_MultipleAccounts(t *testing.T) {
	t.Parallel()

	uri := encodeMigrationURI([]testOtpParam{
		{secret: []byte("secret-one-xxxxxxxxx"), name: "luan.gomes", issuer: "eproc", otpType: 2},
		{secret: []byte("secret-two-xxxxxxxxx"), name: "luan@github", issuer: "GitHub", otpType: 2},
	})

	accounts, err := DecodeMigrationURI(uri)
	if err != nil {
		t.Fatalf("DecodeMigrationURI: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(accounts))
	}
	if accounts[0].Issuer != "eproc" || accounts[1].Issuer != "GitHub" {
		t.Errorf("order/issuers = %q, %q, want eproc, GitHub (encounter order preserved)", accounts[0].Issuer, accounts[1].Issuer)
	}
}

func TestDecodeMigrationURI_UnspecifiedFieldsDefaultToSupported(t *testing.T) {
	// A real export commonly omits algorithm/digits/type entirely when they are
	// the format's own default (SHA1/6-digit/TOTP) — proto3 doesn't encode
	// zero-value fields. Zero everywhere must NOT be rejected.
	t.Parallel()

	uri := encodeMigrationURI([]testOtpParam{{secret: []byte("xxxxxxxxxxxxxxxxxxxx")}})

	accounts, err := DecodeMigrationURI(uri)
	if err != nil {
		t.Fatalf("DecodeMigrationURI: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts))
	}
}

func TestDecodeMigrationURI_UnsupportedFieldsRejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		param testOtpParam
	}{
		{"algorithm SHA256", testOtpParam{secret: []byte("xxxxxxxxxxxxxxxxxxxx"), algorithm: 2}},
		{"digits eight", testOtpParam{secret: []byte("xxxxxxxxxxxxxxxxxxxx"), digits: 2}},
		{"type HOTP", testOtpParam{secret: []byte("xxxxxxxxxxxxxxxxxxxx"), otpType: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			uri := encodeMigrationURI([]testOtpParam{tt.param})
			if _, err := DecodeMigrationURI(uri); err == nil {
				t.Error("want an error for an unsupported field, got nil (would silently generate a WRONG code)")
			}
		})
	}
}

func TestDecodeMigrationURI_EmptySecretRejected(t *testing.T) {
	t.Parallel()
	uri := encodeMigrationURI([]testOtpParam{{name: "no-secret", otpType: 2}})
	if _, err := DecodeMigrationURI(uri); err == nil {
		t.Error("want an error for an account with no secret bytes")
	}
}

func TestDecodeMigrationURI_WrongScheme(t *testing.T) {
	t.Parallel()
	if _, err := DecodeMigrationURI("otpauth://totp/eproc?secret=X"); err == nil {
		t.Error("want an error for a non-migration scheme")
	}
}

func TestDecodeMigrationURI_MissingData(t *testing.T) {
	t.Parallel()
	if _, err := DecodeMigrationURI("otpauth-migration://offline"); err == nil {
		t.Error("want an error when the data param is absent")
	}
}

func TestDecodeMigrationURI_MalformedBase64(t *testing.T) {
	t.Parallel()
	if _, err := DecodeMigrationURI("otpauth-migration://offline?data=%00%01not-base64%02"); err == nil {
		t.Error("want an error for malformed base64")
	}
}

func TestDecodeMigrationURI_TruncatedProtobuf(t *testing.T) {
	t.Parallel()
	// A valid-looking length-delimited tag whose declared length exceeds what
	// actually follows.
	raw := []byte{0x0a, 0xff, 0x01} // field 1, len-delim, length=127 but 0 bytes follow
	data := base64.StdEncoding.EncodeToString(raw)
	uri := "otpauth-migration://offline?data=" + url.QueryEscape(data)
	if _, err := DecodeMigrationURI(uri); err == nil {
		t.Error("want an error for truncated protobuf bytes")
	}
}

func TestDecodeMigrationURI_NoAccountsIsError(t *testing.T) {
	t.Parallel()
	uri := encodeMigrationURI(nil)
	if _, err := DecodeMigrationURI(uri); err == nil {
		t.Error("want an error when the payload carries zero accounts")
	}
}

// A quick smoke test that the RawStdEncoding fallback actually engages: an
// unpadded base64 string that StdEncoding rejects but RawStdEncoding accepts.
func TestDecodeMigrationData_FallsBackToRawEncoding(t *testing.T) {
	t.Parallel()
	raw := encodeOtpParam(testOtpParam{secret: []byte("xxxxxxxxxxxxxxxxxxxx"), otpType: 2})
	payload := encodeLenDelim(nil, 1, raw)
	rawB64 := base64.RawStdEncoding.EncodeToString(payload)
	if strings.HasSuffix(rawB64, "=") {
		t.Fatal("test fixture unexpectedly padded — pick different fixture bytes")
	}

	decoded, err := decodeMigrationData(rawB64)
	if err != nil {
		t.Fatalf("decodeMigrationData: %v", err)
	}
	if string(decoded) != string(payload) {
		t.Error("raw-encoding fallback did not round-trip the payload")
	}
}
