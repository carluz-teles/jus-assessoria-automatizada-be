package totp

import (
	"bytes"
	"image/png"
	"testing"

	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
)

// encodeQRPNG builds a real QR code PNG encoding contents — BitMatrix (the
// encoder's output) already implements image.Image, so no conversion is needed
// before handing it to png.Encode. Round-tripping through a REAL encoder/decoder
// pair (not a hand-built fixture) is what proves DecodeQRImage works against the
// same wire format a phone screenshot produces.
func encodeQRPNG(t *testing.T, contents string) []byte {
	t.Helper()
	matrix, err := qrcode.NewQRCodeWriter().EncodeWithoutHint(contents, gozxing.BarcodeFormat_QR_CODE, 256, 256)
	if err != nil {
		t.Fatalf("encode QR: %v", err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, matrix); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func TestDecodeQRImage_RoundTrips(t *testing.T) {
	const otpauthURI = "otpauth://totp/eproc:luan.gomes?secret=JBSWY3DPEHPK3PXP&issuer=eproc"

	png := encodeQRPNG(t, otpauthURI)

	got, err := DecodeQRImage(png)
	if err != nil {
		t.Fatalf("DecodeQRImage: %v", err)
	}
	if got != otpauthURI {
		t.Fatalf("DecodeQRImage = %q, want %q", got, otpauthURI)
	}
}

func TestDecodeQRImage_NotAnImage(t *testing.T) {
	_, err := DecodeQRImage([]byte("this is not an image"))
	if err == nil {
		t.Fatal("DecodeQRImage(garbage) = nil error, want an error")
	}
}

func TestDecodeQRImage_ImageWithNoQRCode(t *testing.T) {
	// A valid PNG (blank matrix) with no QR code encoded in it at all.
	_, err := DecodeQRImage(mustBlankPNG(t))
	if err == nil {
		t.Fatal("DecodeQRImage(blank image) = nil error, want an error (no QR code present)")
	}
}

func mustBlankPNG(t *testing.T) []byte {
	t.Helper()
	matrix, err := gozxing.NewBitMatrix(64, 64)
	if err != nil {
		t.Fatalf("NewBitMatrix: %v", err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, matrix); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func TestExtractSecret(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "bare manual-entry key",
			input: "JBSWY3DPEHPK3PXP",
			want:  "JBSWY3DPEHPK3PXP",
		},
		{
			name:  "bare key with surrounding whitespace",
			input: "  JBSWY3DPEHPK3PXP  \n",
			want:  "JBSWY3DPEHPK3PXP",
		},
		{
			name:  "otpauth URI with secret param",
			input: "otpauth://totp/eproc:luan.gomes?secret=JBSWY3DPEHPK3PXP&issuer=eproc",
			want:  "JBSWY3DPEHPK3PXP",
		},
		{
			name:    "otpauth URI missing secret param",
			input:   "otpauth://totp/eproc:luan.gomes?issuer=eproc",
			wantErr: true,
		},
		{
			name:    "empty input",
			input:   "   ",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExtractSecret(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ExtractSecret(%q) = %q, nil, want an error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExtractSecret(%q): %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("ExtractSecret(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestExtractAccounts proves the dispatch (migration vs otpauth:// vs bare
// text) — DecodeMigrationURI's own test file covers the migration format's
// internals exhaustively, this only proves ExtractAccounts routes to it
// correctly and wraps the other two shapes as a one-element list.
func TestExtractAccounts(t *testing.T) {
	t.Parallel()

	t.Run("migration URI with a single account", func(t *testing.T) {
		t.Parallel()
		uri := encodeMigrationURI([]testOtpParam{{secret: []byte("xxxxxxxxxxxxxxxxxxxx"), issuer: "eproc", otpType: 2}})

		got, err := ExtractAccounts(uri)
		if err != nil {
			t.Fatalf("ExtractAccounts: %v", err)
		}
		if len(got) != 1 || got[0].Issuer != "eproc" {
			t.Errorf("ExtractAccounts(migration) = %+v, want 1 account issuer=eproc", got)
		}
	})

	t.Run("migration URI with multiple accounts", func(t *testing.T) {
		t.Parallel()
		uri := encodeMigrationURI([]testOtpParam{
			{secret: []byte("secret-one-xxxxxxxxx"), issuer: "eproc", otpType: 2},
			{secret: []byte("secret-two-xxxxxxxxx"), issuer: "GitHub", otpType: 2},
		})

		got, err := ExtractAccounts(uri)
		if err != nil {
			t.Fatalf("ExtractAccounts: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("ExtractAccounts(migration, 2 accounts) = %d accounts, want 2", len(got))
		}
	})

	t.Run("standard otpauth URI wraps as a one-element list", func(t *testing.T) {
		t.Parallel()
		got, err := ExtractAccounts("otpauth://totp/eproc:luan.gomes?secret=JBSWY3DPEHPK3PXP&issuer=eproc")
		if err != nil {
			t.Fatalf("ExtractAccounts: %v", err)
		}
		if len(got) != 1 || got[0].Secret != "JBSWY3DPEHPK3PXP" {
			t.Errorf("ExtractAccounts(otpauth) = %+v, want 1 account secret=JBSWY3DPEHPK3PXP", got)
		}
		if got[0].Name != "" || got[0].Issuer != "" {
			t.Errorf("ExtractAccounts(otpauth) name/issuer = %q/%q, want empty (no disambiguation needed)", got[0].Name, got[0].Issuer)
		}
	})

	t.Run("bare secret wraps as a one-element list", func(t *testing.T) {
		t.Parallel()
		got, err := ExtractAccounts("JBSWY3DPEHPK3PXP")
		if err != nil {
			t.Fatalf("ExtractAccounts: %v", err)
		}
		if len(got) != 1 || got[0].Secret != "JBSWY3DPEHPK3PXP" {
			t.Errorf("ExtractAccounts(bare) = %+v, want 1 account secret=JBSWY3DPEHPK3PXP", got)
		}
	})

	t.Run("empty input still errors", func(t *testing.T) {
		t.Parallel()
		if _, err := ExtractAccounts("   "); err == nil {
			t.Error("ExtractAccounts(empty) = nil error, want an error")
		}
	})
}
