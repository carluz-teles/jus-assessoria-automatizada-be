package certificate

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
)

// Envelope é o resultado do Seal: os 4 pedaços que a linha do PG guarda.
// Ciphertext + Nonce são o AES-GCM local; WrappedDEK é o output do KMS.Encrypt;
// KEKRef é o resource name da key do KMS (guardado pra saber com qual key
// decifrar quando rotate acontecer).
type Envelope struct {
	Ciphertext []byte
	Nonce      []byte
	WrappedDEK []byte
	KEKRef     string
}

// Cipher é a porta que o domain enxerga — abstrai KMS pra testes poderem mockar.
type Cipher interface {
	Seal(ctx context.Context, plaintext []byte) (*Envelope, error)
	Open(ctx context.Context, env *Envelope) ([]byte, error)
	Close() error
}

// EnvelopeCipher usa GCP Cloud KMS como KEK e gera uma DEK aleatória por
// operação. O plaintext (o .pfx) NUNCA sai desta máquina em claro — só a
// DEK, que já vai cifrada, transita entre o api e o KMS.
type EnvelopeCipher struct {
	kmsClient *kms.KeyManagementClient
	kekName   string // ex.: "projects/x/locations/us-c1/keyRings/r/cryptoKeys/k"
}

// NewEnvelopeCipher constrói o cipher com um cliente KMS já autenticado
// (Application Default Credentials + role cloudkms.cryptoKeyEncrypterDecrypter
// na key). Falha rápida no boot se o KMS não responde a um probe (Encrypt de
// 1 byte descartado) — mais barato descobrir key errada aqui do que na 1a request.
func NewEnvelopeCipher(ctx context.Context, kekName string) (*EnvelopeCipher, error) {
	if kekName == "" {
		return nil, errors.New("cert cipher: GCP_KMS_KEY_NAME vazio")
	}
	client, err := kms.NewKeyManagementClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("cert cipher: kms client: %w", err)
	}
	c := &EnvelopeCipher{kmsClient: client, kekName: kekName}
	// probe: 1 chamada Encrypt+Decrypt fajuta. Falha se a key não existe ou
	// a service account não tem permissão — sobe erro claro no boot.
	if _, err := c.kmsClient.Encrypt(ctx, &kmspb.EncryptRequest{
		Name:      kekName,
		Plaintext: []byte{0x01},
	}); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("cert cipher: probe encrypt on %q: %w", kekName, err)
	}
	return c, nil
}

// Close libera o cliente gRPC do KMS. Chamado no shutdown do api.
func (c *EnvelopeCipher) Close() error {
	if c.kmsClient == nil {
		return nil
	}
	return c.kmsClient.Close()
}

// Seal cifra plaintext:
//  1. gera DEK aleatória de 32 bytes (AES-256)
//  2. cifra plaintext localmente com AES-GCM (nonce fresh)
//  3. chama KMS.Encrypt(DEK) → wrapped_dek
//  4. devolve envelope pronto pra persistir
//
// A DEK é zerada da memória antes de retornar (best-effort — Go pode ter
// cópia no GC; hardening real exige mlock/wipe explícito, débito futuro).
func (c *EnvelopeCipher) Seal(ctx context.Context, plaintext []byte) (*Envelope, error) {
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return nil, fmt.Errorf("dek rand: %w", err)
	}
	defer wipe(dek)

	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, fmt.Errorf("aes new: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm new: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("nonce rand: %w", err)
	}
	ct := gcm.Seal(nil, nonce, plaintext, nil)

	// wrap da DEK no KMS.
	resp, err := c.kmsClient.Encrypt(ctx, &kmspb.EncryptRequest{
		Name:      c.kekName,
		Plaintext: dek,
	})
	if err != nil {
		return nil, fmt.Errorf("kms encrypt dek: %w", err)
	}

	return &Envelope{
		Ciphertext: ct,
		Nonce:      nonce,
		WrappedDEK: resp.Ciphertext,
		KEKRef:     c.kekName,
	}, nil
}

// Open decifra o envelope: KMS.Decrypt(wrapped_dek) → DEK → AES-GCM open.
// Erro genérico em tampering pra não vazar motivo pro atacante.
func (c *EnvelopeCipher) Open(ctx context.Context, env *Envelope) ([]byte, error) {
	if env == nil || len(env.WrappedDEK) == 0 || len(env.Nonce) == 0 {
		return nil, errors.New("cert cipher: envelope inválido")
	}
	// Usamos SEMPRE o kekName atual pra decifrar. GCP KMS deriva a versão da
	// key pelo próprio ciphertext (o wrapped_dek carrega qual versão foi usada
	// no encrypt), então rotate-in-place é transparente. env.KEKRef fica pra
	// auditoria e pra o dia que suportarmos MULTI-key (aí passamos env.KEKRef
	// como Name em vez de c.kekName).
	resp, err := c.kmsClient.Decrypt(ctx, &kmspb.DecryptRequest{
		Name:       c.kekName,
		Ciphertext: env.WrappedDEK,
	})
	if err != nil {
		return nil, fmt.Errorf("kms decrypt dek: %w", err)
	}
	dek := resp.Plaintext
	defer wipe(dek)

	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, fmt.Errorf("aes new: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm new: %w", err)
	}
	pt, err := gcm.Open(nil, env.Nonce, env.Ciphertext, nil)
	if err != nil {
		return nil, errors.New("cert cipher: falha ao decifrar")
	}
	return pt, nil
}

// wipe zera o slice — best-effort. Não é wipe cryptographic (Go GC pode ter
// copiado o buffer); mas melhor do que deixar a DEK dependurada.
func wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
