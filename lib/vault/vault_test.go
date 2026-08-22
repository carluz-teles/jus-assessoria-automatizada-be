package vault

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jusassessoria/platform/lib/apperr"
)

// newTestKEK returns a valid base64-encoded 32-byte KEK for the local vault.
func newTestKEK(t *testing.T) string {
	t.Helper()
	kek := make([]byte, kekLen)
	_, err := rand.Read(kek)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(kek)
}

func TestNewLocalVault_ConfigValidation(t *testing.T) {
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

			v, err := NewLocalVault(tt.kek)
			is.Nil(v)
			ae, ok := apperr.From(err)
			is.True(ok)
			is.Equal(apperr.KindInvalid, ae.Kind)
		})
	}
}

func TestLocalVault_WrapUnwrap_RoundTrip(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	v, err := NewLocalVault(newTestKEK(t))
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

func TestLocalVault_Wrap_FreshNoncePerCall(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	v, err := NewLocalVault(newTestKEK(t))
	require.NoError(t, err)

	dek := bytes.Repeat([]byte{0xAB}, 32)
	a, err := v.Wrap(context.Background(), dek)
	require.NoError(t, err)
	b, err := v.Wrap(context.Background(), dek)
	require.NoError(t, err)

	is.False(bytes.Equal(a, b), "same DEK wrapped twice must differ (nonce per call)")
}

func TestLocalVault_Unwrap_WrongKEK_Fails(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	v1, err := NewLocalVault(newTestKEK(t))
	require.NoError(t, err)
	v2, err := NewLocalVault(newTestKEK(t))
	require.NoError(t, err)

	dek := bytes.Repeat([]byte{0x01}, 32)
	wrapped, err := v1.Wrap(context.Background(), dek)
	require.NoError(t, err)

	_, err = v2.Unwrap(context.Background(), wrapped)
	ae, ok := apperr.From(err)
	is.True(ok)
	is.Equal(apperr.KindInfra, ae.Kind)
}

func TestLocalVault_Unwrap_TooShort_Fails(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	v, err := NewLocalVault(newTestKEK(t))
	require.NoError(t, err)

	_, err = v.Unwrap(context.Background(), []byte("short"))
	ae, ok := apperr.From(err)
	is.True(ok)
	is.Equal(apperr.KindInfra, ae.Kind)
}

// fakeKMS is an in-memory KMS double: Encrypt prefixes a marker so ciphertext
// differs from plaintext; Decrypt strips it. It proves the vault threads the
// bytes through the API without hitting AWS.
type fakeKMS struct {
	prefix  []byte
	encErr  error
	decErr  error
	lastKey string
}

func (f *fakeKMS) Encrypt(_ context.Context, in *kms.EncryptInput, _ ...func(*kms.Options)) (*kms.EncryptOutput, error) {
	if f.encErr != nil {
		return nil, f.encErr
	}
	f.lastKey = *in.KeyId
	return &kms.EncryptOutput{CiphertextBlob: append(append([]byte{}, f.prefix...), in.Plaintext...)}, nil
}

func (f *fakeKMS) Decrypt(_ context.Context, in *kms.DecryptInput, _ ...func(*kms.Options)) (*kms.DecryptOutput, error) {
	if f.decErr != nil {
		return nil, f.decErr
	}
	return &kms.DecryptOutput{Plaintext: bytes.TrimPrefix(in.CiphertextBlob, f.prefix)}, nil
}

func TestKMSVault_WrapUnwrap_RoundTrip(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	fake := &fakeKMS{prefix: []byte("WRAPPED:")}
	v := &kmsVault{api: fake, keyID: "arn:aws:kms:us-east-1:1:key/abc"}

	dek := bytes.Repeat([]byte{0x07}, 32)
	wrapped, err := v.Wrap(context.Background(), dek)
	require.NoError(t, err)
	is.False(bytes.Equal(dek, wrapped))
	is.Equal("arn:aws:kms:us-east-1:1:key/abc", fake.lastKey)

	unwrapped, err := v.Unwrap(context.Background(), wrapped)
	require.NoError(t, err)
	is.True(bytes.Equal(dek, unwrapped))
	is.Equal(KekRefKMS+":arn:aws:kms:us-east-1:1:key/abc", v.KekRef())
}

func TestKMSVault_Wrap_Error_IsInfra(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	v := &kmsVault{api: &fakeKMS{encErr: errors.New("kms down")}, keyID: "k"}
	_, err := v.Wrap(context.Background(), []byte("dek"))
	ae, ok := apperr.From(err)
	is.True(ok)
	is.Equal(apperr.KindInfra, ae.Kind)
}

func TestNewKMSVault_ConfigValidation(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	_, err := NewKMSVault(KMSOptions{Region: "us-east-1"}) // no key id
	ae, ok := apperr.From(err)
	is.True(ok)
	is.Equal(apperr.KindInvalid, ae.Kind)

	_, err = NewKMSVault(KMSOptions{KeyID: "k"}) // no region
	ae, ok = apperr.From(err)
	is.True(ok)
	is.Equal(apperr.KindInvalid, ae.Kind)
}
