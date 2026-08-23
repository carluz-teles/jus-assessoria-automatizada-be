package certificate

import (
	"context"
	"time"

	"github.com/jusassessoria/platform/lib/database"
)

// UseCase orquestra os fluxos: preview, upload, list, revoke, sign. NÃO owns tx
// (usa UoW). tenantID + userID sempre vêm do principal verificado, nunca do body.
// Storage foi eliminado do fluxo — o binário .pfx cifrado vive in-row via
// envelope (Cipher.Seal grava; Cipher.Open decifra no Sign).
type UseCase struct {
	repo   Repository
	uow    database.UnitOfWork
	cipher Cipher
}

// NewUseCase constrói o caso de uso. cipher é exigido — o wire no api só chama
// isso quando o GCP KMS tá configurado (senão o slice não sobe).
func NewUseCase(repo Repository, uow database.UnitOfWork, c Cipher) *UseCase {
	return &UseCase{repo: repo, uow: uow, cipher: c}
}

// Preview parseia o .pfx com a senha, devolve metadata + checks. NADA é
// persistido. Erros típicos: senha errada, arquivo corrupto.
func (uc *UseCase) Preview(_ context.Context, pfx []byte, password string) (*PreviewResult, error) {
	p, err := parsePFX(pfx, password)
	if err != nil {
		return nil, err
	}
	meta := toMetadata(p)
	return &PreviewResult{Meta: meta, Checks: checkPFX(meta)}, nil
}

// Upload parseia, valida (não expirado), cifra com envelope (DEK local + KMS wrap)
// e persiste a metadata + envelope numa tx. Retorna o Certificate cadastrado.
// Idempotência: mesmo fingerprint duplicado no tenant ativo devolve
// ErrCertificateAlreadyExists.
func (uc *UseCase) Upload(ctx context.Context, tenantID, ownerUserID string, pfx []byte, password string) (*Certificate, error) {
	p, err := parsePFX(pfx, password)
	if err != nil {
		return nil, err
	}
	meta := toMetadata(p)

	// Validação de domínio: cert expirado NÃO cadastra (proteção UX).
	if time.Now().UTC().After(meta.NotAfter) {
		return nil, ErrCertificateExpired
	}

	// Envelope: DEK aleatória cifra o .pfx localmente; KMS wrappa a DEK.
	env, err := uc.cipher.Seal(ctx, pfx)
	if err != nil {
		return nil, err
	}

	cert := &Certificate{
		TenantID:    tenantID,
		OwnerUserID: ownerUserID,
		SubjectCN:   meta.SubjectCN,
		OAB:         meta.OAB,
		Issuer:      meta.Issuer,
		Serial:      meta.Serial,
		NotBefore:   meta.NotBefore,
		NotAfter:    meta.NotAfter,
		Fingerprint: meta.Fingerprint,
		Envelope:    *env,
	}

	err = uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		id, createdAt, ierr := uc.repo.Insert(ctx, tx, cert)
		if ierr != nil {
			return ierr
		}
		cert.ID = id
		cert.CreatedAt = createdAt
		return nil
	})
	if err != nil {
		return nil, err
	}
	return cert, nil
}

// List devolve os certificados ATIVOS do tenant, com nome do owner. O envelope
// NÃO viaja aqui — só metadata pública.
func (uc *UseCase) List(ctx context.Context, tenantID string) ([]CertificateWithOwner, error) {
	var out []CertificateWithOwner
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		v, e := uc.repo.ListActive(ctx, tx, tenantID)
		if e == nil {
			out = v
		}
		return e
	})
	return out, err
}

// Revoke soft-deleta (marca revoked_at=now()). Idempotente: revogar de novo
// devolve ErrCertificateNotFound (já não está ativo).
func (uc *UseCase) Revoke(ctx context.Context, tenantID, id string) error {
	return uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		return uc.repo.Revoke(ctx, tx, tenantID, id)
	})
}

// Sign assina um digest SHA-256 com a chave do certificado. Fluxo: busca cert
// (metadata + envelope); Cipher.Open (KMS.Decrypt na DEK, AES-GCM local); abre
// PKCS#12 com a senha do user; assina digest; descarta chave.
func (uc *UseCase) Sign(ctx context.Context, tenantID, id, password string, digest []byte) (*SignResult, error) {
	var cert *Certificate
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		c, e := uc.repo.GetByID(ctx, tx, tenantID, id)
		if e != nil {
			return e
		}
		if c.RevokedAt != nil {
			return ErrCertificateNotFound
		}
		cert = c
		return nil
	})
	if err != nil {
		return nil, err
	}

	pfx, err := uc.cipher.Open(ctx, &cert.Envelope)
	if err != nil {
		return nil, err
	}
	p, err := parsePFX(pfx, password)
	if err != nil {
		return nil, err
	}
	sig, err := signSHA256(p, digest)
	if err != nil {
		return nil, err
	}
	chain := make([][]byte, 0, len(p.Chain))
	for _, cc := range p.Chain {
		chain = append(chain, cc.Raw)
	}
	return &SignResult{Signature: sig, Chain: chain}, nil
}
