package totp

import (
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
)

// Account is one TOTP entry extracted from an otpauth-migration:// URI — the
// format Google Authenticator's own "Export accounts" feature produces
// (Settings → Transfer accounts → Export accounts), NOT the eproc-specific
// enrollment/reconfiguration QR. This is the mechanism the competitor (Legal
// Mail) uses: the lawyer exports their EXISTING authenticator entries —
// including one already configured for eproc — without touching eproc's own
// 2FA settings at all, so there is no reset risk (resetting would break that
// same seed's use anywhere else it's already configured).
type Account struct {
	Secret string // base32, unpadded — ready for GenerateCode
	Name   string
	Issuer string
}

// Wire types this file's minimal protobuf reader understands — the migration
// schema below uses only these two (no oneof/map/extension needs a third).
const (
	wireVarint   = 0
	wireLenDelim = 2
)

// DecodeMigrationURI parses an "otpauth-migration://offline?data=..." URI into
// its Account entries. The payload (confirmed against the public reverse-
// engineered schema, e.g. github.com/dim13/otpauth) is:
//
//	message Payload {
//	  message OtpParameters {
//	    bytes secret = 1; string name = 2; string issuer = 3;
//	    Algorithm algorithm = 4;  // 0=unspecified 1=SHA1 2=SHA256 3=SHA512 4=MD5
//	    DigitCount digits = 5;    // 0=unspecified 1=SIX 2=EIGHT
//	    OtpType type = 6;         // 0=unspecified 1=HOTP 2=TOTP
//	    uint64 counter = 7; string unique_id = 8;
//	  }
//	  repeated OtpParameters otp_parameters = 1;
//	  int32 version = 2; int32 batch_size = 3; int32 batch_index = 4; int32 batch_id = 5;
//	}
//
// Hand-rolled against the raw protobuf wire format (varint + length-delimited
// only) instead of pulling in protoc/protoc-gen-go for one fixed, never-
// changing message — same "a little copying beats a little dependency" call
// this package already made for RFC 6238 itself.
//
// ASSUMPTION (Portão B, not yet confirmed against a real phone export — same
// discipline as lib/eproc/wiring.go): the "data" query param is this Payload
// serialized then standard base64. Publicly documented captures of this format
// agree, but confirm against an actual lawyer's export before treating this
// path as CONFIRMED.
func DecodeMigrationURI(uri string) ([]Account, error) {
	u, err := url.Parse(strings.TrimSpace(uri))
	if err != nil {
		return nil, fmt.Errorf("totp: parse migration URI: %w", err)
	}
	if !strings.EqualFold(u.Scheme, "otpauth-migration") {
		return nil, fmt.Errorf("totp: not a migration URI (scheme %q)", u.Scheme)
	}
	data := u.Query().Get("data")
	if data == "" {
		return nil, ErrSecretNotFound
	}

	raw, err := decodeMigrationData(data)
	if err != nil {
		return nil, fmt.Errorf("totp: decode migration data: %w", err)
	}

	params, err := parseOtpParametersList(raw)
	if err != nil {
		return nil, fmt.Errorf("totp: parse migration payload: %w", err)
	}
	if len(params) == 0 {
		return nil, ErrSecretNotFound
	}

	accounts := make([]Account, 0, len(params))
	for i, p := range params {
		if err := p.validate(); err != nil {
			return nil, fmt.Errorf("totp: account %d (%s): %w", i, p.name, err)
		}
		accounts = append(accounts, Account{
			Secret: base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(p.secret),
			Name:   p.name,
			Issuer: p.issuer,
		})
	}
	return accounts, nil
}

// decodeMigrationData tries standard base64 first, then the unpadded variant —
// real captures of this format vary depending on how the value survived a
// copy/paste or a URL re-encoding step before reaching us.
func decodeMigrationData(data string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(data); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(data)
}

// otpParameters is the raw decoded form of one OtpParameters submessage —
// unexported; converted to the public Account only after validate() passes.
type otpParameters struct {
	secret    []byte
	name      string
	issuer    string
	algorithm uint64
	digitsN   uint64
	otpType   uint64
}

// validate rejects anything GenerateCode (hardcoded HMAC-SHA1, 6 digits, TOTP —
// see totp.go) cannot honor. Silently accepting a mismatched algorithm/digit
// count would produce a plausible-looking but WRONG 6-digit code — the worst
// failure mode for a security feature, so this fails loud instead.
func (p otpParameters) validate() error {
	// 0 = unspecified (the format's own default, meaning SHA1/6-digit/TOTP).
	if p.algorithm != 0 && p.algorithm != 1 {
		return fmt.Errorf("algoritmo TOTP não suportado (código %d, só SHA1)", p.algorithm)
	}
	if p.digitsN != 0 && p.digitsN != 1 {
		return fmt.Errorf("quantidade de dígitos não suportada (código %d, só 6)", p.digitsN)
	}
	if p.otpType != 0 && p.otpType != 2 {
		return fmt.Errorf("tipo de OTP não suportado (código %d, só TOTP)", p.otpType)
	}
	if len(p.secret) == 0 {
		return ErrSecretNotFound
	}
	return nil
}

// parseOtpParametersList decodes a top-level Payload message, returning its
// repeated otp_parameters (field 1) submessages in order. Every other field
// (version/batch_*) is skipped — irrelevant here.
func parseOtpParametersList(data []byte) ([]otpParameters, error) {
	var out []otpParameters
	for len(data) > 0 {
		fieldNum, wireType, rest, err := readTag(data)
		if err != nil {
			return nil, err
		}
		data = rest

		switch wireType {
		case wireVarint:
			_, rest, err := readVarint(data)
			if err != nil {
				return nil, err
			}
			data = rest
		case wireLenDelim:
			payload, rest, err := readLenDelim(data)
			if err != nil {
				return nil, err
			}
			data = rest
			if fieldNum == 1 {
				p, err := parseOtpParameters(payload)
				if err != nil {
					return nil, err
				}
				out = append(out, p)
			}
		default:
			return nil, fmt.Errorf("unsupported wire type %d (field %d)", wireType, fieldNum)
		}
	}
	return out, nil
}

// parseOtpParameters decodes one OtpParameters submessage.
func parseOtpParameters(data []byte) (otpParameters, error) {
	var p otpParameters
	for len(data) > 0 {
		fieldNum, wireType, rest, err := readTag(data)
		if err != nil {
			return p, err
		}
		data = rest

		switch wireType {
		case wireVarint:
			v, rest, err := readVarint(data)
			if err != nil {
				return p, err
			}
			data = rest
			switch fieldNum {
			case 4:
				p.algorithm = v
			case 5:
				p.digitsN = v
			case 6:
				p.otpType = v
			}
		case wireLenDelim:
			payload, rest, err := readLenDelim(data)
			if err != nil {
				return p, err
			}
			data = rest
			switch fieldNum {
			case 1:
				p.secret = payload
			case 2:
				p.name = string(payload)
			case 3:
				p.issuer = string(payload)
			}
		default:
			return p, fmt.Errorf("unsupported wire type %d (field %d)", wireType, fieldNum)
		}
	}
	return p, nil
}

// readTag reads a protobuf tag (field_number<<3 | wire_type) from the start of
// data, returning the decoded parts and the remaining bytes.
func readTag(data []byte) (fieldNum int, wireType int, rest []byte, err error) {
	v, rest, err := readVarint(data)
	if err != nil {
		return 0, 0, nil, err
	}
	return int(v >> 3), int(v & 0x7), rest, nil
}

// readVarint reads a base-128 varint from the start of data.
func readVarint(data []byte) (value uint64, rest []byte, err error) {
	var shift uint
	for i := 0; i < len(data); i++ {
		b := data[i]
		if shift >= 64 {
			return 0, nil, fmt.Errorf("varint too long")
		}
		value |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return value, data[i+1:], nil
		}
		shift += 7
	}
	return 0, nil, fmt.Errorf("truncated varint")
}

// readLenDelim reads a length-prefixed byte slice (a protobuf wire-type-2
// field's payload) from the start of data.
func readLenDelim(data []byte) (payload []byte, rest []byte, err error) {
	n, rest, err := readVarint(data)
	if err != nil {
		return nil, nil, err
	}
	if uint64(len(rest)) < n {
		return nil, nil, fmt.Errorf("truncated length-delimited field (want %d bytes, have %d)", n, len(rest))
	}
	return rest[:n], rest[n:], nil
}
