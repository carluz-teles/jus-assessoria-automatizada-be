package vault_test

import (
	"encoding/base64"
	"errors"
	"testing"

	"github.com/jusassessoria/platform/lib/vault"
)

// testKEK returns a fresh, valid base64-encoded 32-byte KEK for each test that
// needs one, so tests never share key material.
func testKEK(t *testing.T) string {
	t.Helper()
	kek, err := vault.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK() error = %v", err)
	}
	return kek
}

func TestNew_ValidatesKEK(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		kek     func(t *testing.T) string
		wantErr bool
	}{
		{
			name:    "valid 32-byte base64 key",
			kek:     testKEK,
			wantErr: false,
		},
		{
			name:    "not base64",
			kek:     func(*testing.T) string { return "not-valid-base64!!!" },
			wantErr: true,
		},
		{
			name:    "wrong length (too short)",
			kek:     func(*testing.T) string { return base64.StdEncoding.EncodeToString([]byte("too-short")) },
			wantErr: true,
		},
		{
			name:    "wrong length (too long)",
			kek:     func(*testing.T) string { return base64.StdEncoding.EncodeToString(make([]byte, 64)) },
			wantErr: true,
		},
		{
			name:    "empty",
			kek:     func(*testing.T) string { return "" },
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := vault.New(tt.kek(t))
			if tt.wantErr && err == nil {
				t.Fatal("New() error = nil, want error")
			}
			if tt.wantErr && !errors.Is(err, vault.ErrInvalidKEK) {
				t.Errorf("New() error = %v, want wrapping ErrInvalidKEK", err)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("New() error = %v, want nil", err)
			}
		})
	}
}

func TestVault_SealOpen_RoundTrips(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		plaintext string
	}{
		{name: "typical password", plaintext: "S3nh4Sup3rS3cr3ta!2026"},
		{name: "empty string", plaintext: ""},
		{name: "unicode", plaintext: "sênhã-cøm-açêntø-🔒"},
		{name: "long value", plaintext: string(make([]byte, 4096))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			v, err := vault.New(testKEK(t))
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			sealed, err := v.Seal(tt.plaintext)
			if err != nil {
				t.Fatalf("Seal() error = %v", err)
			}

			got, err := v.Open(sealed)
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			if got != tt.plaintext {
				t.Errorf("Open() = %q, want %q", got, tt.plaintext)
			}
		})
	}
}

func TestVault_Seal_NeverLeaksPlaintextInSealedBytes(t *testing.T) {
	t.Parallel()

	v, err := vault.New(testKEK(t))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	const plaintext = "senha-do-advogado-nunca-em-claro"
	sealed, err := v.Seal(plaintext)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}

	for name, b := range map[string][]byte{
		"Ciphertext":    sealed.Ciphertext,
		"DEKCiphertext": sealed.DEKCiphertext,
	} {
		if string(b) == plaintext {
			t.Errorf("%s equals the plaintext verbatim — envelope encryption did not run", name)
		}
	}
}

func TestVault_Seal_IsNonDeterministic(t *testing.T) {
	t.Parallel()

	v, err := vault.New(testKEK(t))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	const plaintext = "mesma-senha-duas-vezes"
	first, err := v.Seal(plaintext)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	second, err := v.Seal(plaintext)
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}

	if string(first.Ciphertext) == string(second.Ciphertext) {
		t.Error("two seals of the same plaintext produced identical ciphertext — DEK is not fresh per call")
	}
	if string(first.DEKCiphertext) == string(second.DEKCiphertext) {
		t.Error("two seals of the same plaintext produced identical wrapped DEK — DEK is not fresh per call")
	}
}

func TestVault_Open_RejectsTamperedCiphertext(t *testing.T) {
	t.Parallel()

	v, err := vault.New(testKEK(t))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	sealed, err := v.Seal("senha-original")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}

	tampered := sealed
	tampered.Ciphertext = append([]byte(nil), sealed.Ciphertext...)
	tampered.Ciphertext[0] ^= 0xFF

	if _, err := v.Open(tampered); !errors.Is(err, vault.ErrSealedSecretTampered) {
		t.Errorf("Open(tampered ciphertext) error = %v, want ErrSealedSecretTampered", err)
	}
}

func TestVault_Open_RejectsTamperedWrappedDEK(t *testing.T) {
	t.Parallel()

	v, err := vault.New(testKEK(t))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	sealed, err := v.Seal("senha-original")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}

	tampered := sealed
	tampered.DEKCiphertext = append([]byte(nil), sealed.DEKCiphertext...)
	tampered.DEKCiphertext[0] ^= 0xFF

	if _, err := v.Open(tampered); !errors.Is(err, vault.ErrSealedSecretTampered) {
		t.Errorf("Open(tampered wrapped DEK) error = %v, want ErrSealedSecretTampered", err)
	}
}

func TestVault_Open_FailsWithWrongKEK(t *testing.T) {
	t.Parallel()

	sealer, err := vault.New(testKEK(t))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	opener, err := vault.New(testKEK(t)) // different KEK
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	sealed, err := sealer.Seal("senha-original")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}

	if _, err := opener.Open(sealed); !errors.Is(err, vault.ErrSealedSecretTampered) {
		t.Errorf("Open() with wrong KEK error = %v, want ErrSealedSecretTampered", err)
	}
}

func TestGenerateKEK_ProducesValidKEK(t *testing.T) {
	t.Parallel()

	kek, err := vault.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK() error = %v", err)
	}

	if _, err := vault.New(kek); err != nil {
		t.Errorf("New(GenerateKEK()) error = %v, want nil (GenerateKEK must produce a valid KEK)", err)
	}
}
