package draft

import (
	"context"
	"time"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
)

// ── SecretVault (KMS envelope) — Fatia 1 peticionamento automático ──────────
//
// A senha do e-SAJ é cifrada com o MESMO envelope KMS usado em certificate
// (SecretVault via structural typing). certificate.Cipher satisfaz esta
// interface; o adapter em cmd/api converte os tipos de Envelope (evita import
// cíclico: draft não conhece certificate). A senha NUNCA persiste em claro.

// Envelope é o resultado do Seal: os 4 pedaços que a linha do PG guarda.
type Envelope struct {
	Ciphertext []byte
	Nonce      []byte
	WrappedDEK []byte
	KEKRef     string
}

// SecretVault é a porta que o domain enxerga — abstrai o KMS pra testes
// poderem mockar (igual a certificate.Cipher).
type SecretVault interface {
	Seal(ctx context.Context, plaintext []byte) (*Envelope, error)
	Open(ctx context.Context, env *Envelope) ([]byte, error)
	Close() error
}

// EsajCredential é a credencial e-SAJ (login + senha cifrada) de um advogado.
// O envelope (ciphertext/nonce/wrapped_dek/kek_ref) NUNCA viaja na entidade de
// listagem — só metadata pública + o consentimento dos termos.
type EsajCredential struct {
	ID              string
	TenantID        string
	OwnerUserID     string
	Login           string
	TermsVersion    string
	TermsAcceptedAt time.Time
	CreatedAt       time.Time
}

// EsajCredentialEnvelope é a credencial com o envelope KMS — só o worker (que vai
// decifrar a senha em memória) a recebe. Nunca exposta ao handler/FE.
type EsajCredentialEnvelope struct {
	ID         string
	Login      string
	Ciphertext []byte
	Nonce      []byte
	WrappedDEK []byte
	KEKRef     string
}

// UploadEsajCredentialCommand é o input do handler (validado via ozzo-validation).
type UploadEsajCredentialCommand struct {
	TenantID        string
	OwnerUserID     string // do principal, nunca do body
	Login           string
	Password        string
	TermsVersion    string
	TermsAcceptedAt time.Time
	TermsAcceptedBy string // do principal (quem deu o consentimento)
}

// UploadEsajCredential cadastra a credencial e-SAJ: cifra a senha no envelope KMS
// e persiste (login + envelope + consentimento). O unique parcial
// (tenant_id, owner_user_id) WHERE ativo impede 2 credenciais ativas — um novo
// upload revoga a anterior (o handler faz o revoke da antiga antes, no fluxo FE).
func (uc *UseCase) UploadEsajCredential(ctx context.Context, cmd UploadEsajCredentialCommand) (*EsajCredential, error) {
	if uc.secretVault == nil {
		return nil, ErrSecretVaultUnavailable
	}
	if cmd.Login == "" {
		return nil, apperr.NewInvalid("login do e-SAJ é obrigatório")
	}
	if cmd.Password == "" {
		return nil, apperr.NewInvalid("senha do e-SAJ é obrigatória")
	}
	if cmd.TermsVersion == "" {
		return nil, apperr.NewInvalid("versão dos termos é obrigatória")
	}

	env, err := uc.secretVault.Seal(ctx, []byte(cmd.Password))
	if err != nil {
		return nil, apperr.NewInfra("seal esaj password", err)
	}

	var cred *EsajCredential
	err = uc.repo.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		row, e := uc.rw.InsertEsajCredential(ctx, tx, cmd.TenantID, cmd.OwnerUserID, cmd.Login, env, cmd.TermsVersion, cmd.TermsAcceptedAt.Format(time.RFC3339), cmd.TermsAcceptedBy)
		if e != nil {
			return e
		}
		cred = row
		return nil
	})
	if err != nil {
		return nil, err
	}
	return cred, nil
}

// ListEsajCredentials devolve as credenciais ATIVAS do tenant (metadata, sem envelope).
func (uc *UseCase) ListEsajCredentials(ctx context.Context, tenantID string) ([]EsajCredential, error) {
	var out []EsajCredential
	err := uc.repo.Do(ctx, tenantID, func(tx database.Tx) error {
		rows, e := uc.rw.ListEsajCredentials(ctx, tx, tenantID)
		if e != nil {
			return e
		}
		out = rows
		return nil
	})
	return out, err
}

// RevokeEsajCredential revoga (soft-delete) a credencial do tenant. Idempotente:
// revogar de novo é no-op (a query só afeta quando revoked_at IS NULL).
func (uc *UseCase) RevokeEsajCredential(ctx context.Context, tenantID, id string) error {
	return uc.repo.Do(ctx, tenantID, func(tx database.Tx) error {
		return uc.rw.RevokeEsajCredential(ctx, tx, tenantID, id)
	})
}

// openEsajCredential carrega a credencial ativa do usuário e decifra a senha em
// memória (nunca trafega em claro no Redis/DB). Free function compartilhada entre
// o UseCase e o FilingUseCase (worker) — fonte única da lógica de decifragem.
func openEsajCredential(ctx context.Context, tx database.Tx, rw Repository, vault SecretVault, tenantID, ownerUserID string) (login, password string, err error) {
	if vault == nil {
		return "", "", ErrSecretVaultUnavailable
	}
	row, err := rw.GetActiveEsajCredential(ctx, tx, tenantID, ownerUserID)
	if err != nil {
		if isNoRows(err) {
			return "", "", ErrEsajCredentialNotFound
		}
		return "", "", err
	}
	pw, err := vault.Open(ctx, &Envelope{
		Ciphertext: row.Ciphertext,
		Nonce:      row.Nonce,
		WrappedDEK: row.WrappedDEK,
		KEKRef:     row.KEKRef,
	})
	if err != nil {
		return "", "", apperr.NewInfra("open esaj password", err)
	}
	return row.Login, string(pw), nil
}

// openEsajCredentialFor é o atalho do UseCase (usa seus próprios ports).
func (uc *UseCase) openEsajCredential(ctx context.Context, tx database.Tx, tenantID, ownerUserID string) (string, string, error) {
	return openEsajCredential(ctx, tx, uc.rw, uc.secretVault, tenantID, ownerUserID)
}
