package vault

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/kms"

	"github.com/jusassessoria/platform/lib/apperr"
)

// kmsAPI is the narrow slice of the KMS client the vault needs — just Encrypt and
// Decrypt of a data key. Defined consumer-side so a fake substitutes for it in
// tests without touching AWS; *kms.Client satisfies it structurally.
type kmsAPI interface {
	Encrypt(ctx context.Context, in *kms.EncryptInput, optFns ...func(*kms.Options)) (*kms.EncryptOutput, error)
	Decrypt(ctx context.Context, in *kms.DecryptInput, optFns ...func(*kms.Options)) (*kms.DecryptOutput, error)
}

// kmsVault wraps a DEK by encrypting it under a KMS customer master key (CMK):
// Wrap is kms.Encrypt(DEK), Unwrap is kms.Decrypt(wrappedDEK). The plaintext CMK
// never leaves AWS; only the DEK crosses the wire, and only ever encrypted. This
// is the PRODUCTION vault, selected when AWS_KMS_KEY_ID is set.
type kmsVault struct {
	api   kmsAPI
	keyID string
}

var _ SecretVault = (*kmsVault)(nil)

// KMSOptions carries what NewKMSVault needs to reach KMS. Endpoint is for
// LocalStack/testing (empty = real AWS). AccessKey/SecretKey are optional: left
// empty, the SDK's default credential chain (IAM role, env, shared config)
// applies — the production path on Railway/AWS.
type KMSOptions struct {
	KeyID     string
	Region    string
	Endpoint  string
	AccessKey string
	SecretKey string
}

// NewKMSVault builds a KMS-backed vault bound to opts.KeyID. A missing key id or
// region is a caller configuration mistake (apperr.Invalid); construction touches
// no network. When both static credentials are supplied they are used; otherwise
// the SDK's default credential chain resolves them.
func NewKMSVault(opts KMSOptions) (SecretVault, error) {
	if opts.KeyID == "" {
		return nil, apperr.NewInvalid("vault: AWS_KMS_KEY_ID is required for the kms vault")
	}
	if opts.Region == "" {
		return nil, apperr.NewInvalid("vault: region is required for the kms vault")
	}

	cfg := aws.Config{Region: opts.Region}
	if opts.AccessKey != "" && opts.SecretKey != "" {
		cfg.Credentials = credentials.NewStaticCredentialsProvider(opts.AccessKey, opts.SecretKey, "")
	}

	endpoint := opts.Endpoint
	api := kms.NewFromConfig(cfg, func(o *kms.Options) {
		if endpoint != "" {
			o.BaseEndpoint = &endpoint
		}
	})

	return &kmsVault{api: api, keyID: opts.KeyID}, nil
}

// Wrap encrypts the plaintext DEK under the CMK. A KMS failure is an infra error
// — the caller (the certificate use case) surfaces it as a 500, never exposing
// the KMS detail.
func (v *kmsVault) Wrap(ctx context.Context, plaintextDEK []byte) ([]byte, error) {
	out, err := v.api.Encrypt(ctx, &kms.EncryptInput{
		KeyId:     &v.keyID,
		Plaintext: plaintextDEK,
	})
	if err != nil {
		return nil, apperr.NewInfra("vault: kms encrypt dek", err)
	}
	return out.CiphertextBlob, nil
}

// Unwrap decrypts a wrapped DEK. KMS resolves the CMK from the ciphertext itself,
// so no key id is needed here. A failure is an infra error.
func (v *kmsVault) Unwrap(ctx context.Context, wrappedDEK []byte) ([]byte, error) {
	out, err := v.api.Decrypt(ctx, &kms.DecryptInput{
		CiphertextBlob: wrappedDEK,
	})
	if err != nil {
		return nil, apperr.NewInfra("vault: kms decrypt dek", err)
	}
	return out.Plaintext, nil
}

// KekRef reports the KMS wrapping-key label with the CMK id, so a rotated
// deployment can tell which key wrapped an old record. The CMK id (an ARN or key
// id) is not a secret.
func (v *kmsVault) KekRef() string { return KekRefKMS + ":" + v.keyID }
