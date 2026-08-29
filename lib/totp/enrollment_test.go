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
