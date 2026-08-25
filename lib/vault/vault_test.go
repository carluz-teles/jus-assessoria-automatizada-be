package vault

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jusassessoria/platform/lib/apperr"
)

// newTestKEK returns a valid base64-encoded 32-byte KEK for tests.
func newTestKEK(t *testing.T) string {
	t.Helper()
	kek := make([]byte, keySize)
	_, err := rand.Read(kek)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(kek)
}

func TestNew_ConfigValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kek  string
	}{
		{name: "empty", kek: ""},
		{name: "not base64", kek: "!!!not-base64!!!"},
		{name: "wrong length", kek: base64.StdEncoding.EncodeToString([]byte("too-short"))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			is := assert.New(t)

			v, err := New(tt.kek)
			is.Nil(v)
			ae, ok := apperr.From(err)
			is.True(ok)
			is.Equal(apperr.KindInvalid, ae.Kind)
		})
	}
}

func TestVault_SealOpen_RoundTrip(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	v, err := New(newTestKEK(t))
	require.NoError(t, err)

	dek := make([]byte, 32)
	_, err = rand.Read(dek)
	require.NoError(t, err)

	sealed, err := v.Seal(string(dek))
	require.NoError(t, err)

	got, err := v.Open(sealed)
	require.NoError(t, err)
	is.Equal(string(dek), got)
}

func TestVault_Seal_NeverLeaksPlaintextInSealedBytes(t *testing.T) {
	t.Parallel()

	v, err := New(newTestKEK(t))
	require.NoError(t, err)

	const plaintext = "senha-do-advogado-nunca-em-claro"
	sealed, err := v.Seal(plaintext)
	require.NoError(t, err)

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

	v, err := New(newTestKEK(t))
	require.NoError(t, err)

	const plaintext = "mesma-senha-duas-vezes"
	first, err := v.Seal(plaintext)
	require.NoError(t, err)
	second, err := v.Seal(plaintext)
	require.NoError(t, err)

	if string(first.Ciphertext) == string(second.Ciphertext) {
		t.Error("two seals of the same plaintext produced identical ciphertext — DEK is not fresh per call")
	}
	if string(first.DEKCiphertext) == string(second.DEKCiphertext) {
		t.Error("two seals of the same plaintext produced identical wrapped DEK — DEK is not fresh per call")
	}
}

func TestVault_Open_RejectsTamperedCiphertext(t *testing.T) {
	t.Parallel()

	v, err := New(newTestKEK(t))
	require.NoError(t, err)

	sealed, err := v.Seal("senha-original")
	require.NoError(t, err)

	tampered := sealed
	tampered.Ciphertext = append([]byte(nil), sealed.Ciphertext...)
	tampered.Ciphertext[0] ^= 0xFF

	if _, err := v.Open(tampered); !errors.Is(err, ErrSealedSecretTampered) {
		t.Errorf("Open(tampered ciphertext) error = %v, want ErrSealedSecretTampered", err)
	}
}

func TestVault_Open_RejectsTamperedWrappedDEK(t *testing.T) {
	t.Parallel()

	v, err := New(newTestKEK(t))
	require.NoError(t, err)

	sealed, err := v.Seal("senha-original")
	require.NoError(t, err)

	tampered := sealed
	tampered.DEKCiphertext = append([]byte(nil), sealed.DEKCiphertext...)
	tampered.DEKCiphertext[0] ^= 0xFF

	if _, err := v.Open(tampered); !errors.Is(err, ErrSealedSecretTampered) {
		t.Errorf("Open(tampered wrapped DEK) error = %v, want ErrSealedSecretTampered", err)
	}
}

func TestVault_Open_FailsWithWrongKEK(t *testing.T) {
	t.Parallel()

	sealer, err := New(newTestKEK(t))
	require.NoError(t, err)
	opener, err := New(newTestKEK(t)) // different KEK
	require.NoError(t, err)

	sealed, err := sealer.Seal("senha-original")
	require.NoError(t, err)

	if _, err := opener.Open(sealed); !errors.Is(err, ErrSealedSecretTampered) {
		t.Errorf("Open() with wrong KEK error = %v, want ErrSealedSecretTampered", err)
	}
}

func TestVault_WrapUnwrap_RoundTrip(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	v, err := New(newTestKEK(t))
	require.NoError(t, err)

	dek := make([]byte, 32)
	_, err = rand.Read(dek)
	require.NoError(t, err)

	wrapped, err := v.Wrap(context.Background(), dek)
	require.NoError(t, err)
	is.NotEqual(dek, wrapped, "wrapped DEK must not equal the plaintext DEK")

	unwrapped, err := v.Unwrap(context.Background(), wrapped)
	require.NoError(t, err)
	is.True(bytes.Equal(dek, unwrapped), "unwrapped DEK must round-trip to the original")
	is.Equal(KekRefLocal, v.KekRef())
}

func TestVault_Wrap_FreshNoncePerCall(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	v, err := New(newTestKEK(t))
	require.NoError(t, err)

	dek := bytes.Repeat([]byte{0xAB}, 32)
	a, err := v.Wrap(context.Background(), dek)
	require.NoError(t, err)
	b, err := v.Wrap(context.Background(), dek)
	require.NoError(t, err)

	is.False(bytes.Equal(a, b), "same DEK wrapped twice must differ (nonce per call)")
}

func TestVault_Unwrap_WrongKEK_Fails(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	v1, err := New(newTestKEK(t))
	require.NoError(t, err)
	v2, err := New(newTestKEK(t))
	require.NoError(t, err)

	dek := bytes.Repeat([]byte{0x01}, 32)
	wrapped, err := v1.Wrap(context.Background(), dek)
	require.NoError(t, err)

	_, err = v2.Unwrap(context.Background(), wrapped)
	ae, ok := apperr.From(err)
	is.True(ok)
	is.Equal(apperr.KindInfra, ae.Kind)
}

func TestVault_Unwrap_TooShort_Fails(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	v, err := New(newTestKEK(t))
	require.NoError(t, err)

	_, err = v.Unwrap(context.Background(), []byte("short"))
	ae, ok := apperr.From(err)
	is.True(ok)
	is.Equal(apperr.KindInfra, ae.Kind)
}

func TestVault_SealOpen_TamperedCiphertext(t *testing.T) {
	t.Parallel()

	v, err := New(newTestKEK(t))
	require.NoError(t, err)

	sealed, err := v.Seal("senha-original")
	require.NoError(t, err)

	tampered := sealed
	tampered.Ciphertext[0] ^= 0xFF

	if _, err := v.Open(tampered); !errors.Is(err, ErrSealedSecretTampered) {
		t.Errorf("Open(tampered) error = %v, want ErrSealedSecretTampered", err)
	}
}

func TestVault_SealOpen_TamperedWrappedDEK(t *testing.T) {
	t.Parallel()

	v, err := New(newTestKEK(t))
	require.NoError(t, err)

	sealed, err := v.Seal("senha-original")
	require.NoError(t, err)

	tampered := sealed
	tampered.DEKCiphertext[0] ^= 0xFF

	if _, err := v.Open(tampered); !errors.Is(err, ErrSealedSecretTampered) {
		t.Errorf("Open(tampered DEK) error = %v, want ErrSealedSecretTampered", err)
	}
}

func TestVault_SealOpen_WrongKEK(t *testing.T) {
	t.Parallel()

	s, err := New(newTestKEK(t))
	require.NoError(t, err)
	o, err := New(newTestKEK(t))
	require.NoError(t, err)

	sealed, err := s.Seal("senha")
	require.NoError(t, err)

	if _, err := o.Open(sealed); !errors.Is(err, ErrSealedSecretTampered) {
		t.Errorf("Open(wrong KEK) error = %v, want ErrSealedSecretTampered", err)
	}
}

func TestGenerateKEK_ProducesValidKEK(t *testing.T) {
	t.Parallel()

	kek, err := GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK() error = %v", err)
	}

	if _, err := New(kek); err != nil {
		t.Errorf("New(GenerateKEK()) error = %v, want nil (GenerateKEK must produce a valid KEK)", err)
	}
}

func TestVault_SealOpen_RoundTrips_Strings(t *testing.T) {
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

			v, err := New(newTestKEK(t))
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
