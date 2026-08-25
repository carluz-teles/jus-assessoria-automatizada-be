package certificate

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// publisher is the narrow outbox port — the producer half of the transactional
// outbox. *events.Outbox satisfies it structurally.
type publisher interface {
	Publish(ctx context.Context, tx database.Tx, ev events.Event) error
}

// UseCase orquestra os fluxos: preview, upload, list, revoke, sign. NÃO owns tx
// (usa UoW). tenantID + userID sempre vêm do principal verificado, nunca do body.
// Storage foi eliminado do fluxo — o binário .pfx cifrado vive in-row via
// envelope (Cipher.Seal grava; Cipher.Open decifra no Sign).
type UseCase struct {
	repo   Repository
	uow    database.UnitOfWork
	cipher Cipher
	outbox publisher
}

// NewUseCase constrói o caso de uso. cipher é exigido — o wire no api só chama
// isso quando o GCP KMS tá configurado (senão o slice não sobe).
func NewUseCase(repo Repository, uow database.UnitOfWork, c Cipher, outbox publisher) *UseCase {
	return &UseCase{repo: repo, uow: uow, cipher: c, outbox: outbox}
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

	// Envelope: cifra {pfx, password} juntos como blob JSON. Trade-off consciente
	// (Fatia 2b, ver commit): armazenar a senha permite assinar sem re-digitação
	// por parte do advogado — 1 clique. Perda: se o api for comprometido, o
	// atacante consegue assinar sem prova de posse humana. Mitigação prevista:
	// audit log por assinatura + rate limit. Prior art: DocuSign REST usa mesma
	// abordagem quando o usuário opta por "remember password".
	blob, err := json.Marshal(vaultBlob{
		PFXBase64: base64.StdEncoding.EncodeToString(pfx),
		Password:  password,
	})
	if err != nil {
		return nil, err
	}
	env, err := uc.cipher.Seal(ctx, blob)
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
		return uc.outbox.Publish(ctx, tx, newCertificateAdded(cert))
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
		if err := uc.repo.Revoke(ctx, tx, tenantID, id); err != nil {
			return err
		}
		return uc.outbox.Publish(ctx, tx, newCertificateRevoked(tenantID, id))
	})
}

// vaultBlob é o payload interno cifrado pelo envelope: {pfx, password} juntos
// como JSON. NUNCA vaza pra fora do pacote — só o Sign/openVault manipulam.
type vaultBlob struct {
	PFXBase64 string `json:"pfx"`      // .pfx bytes em base64
	Password  string `json:"password"` // senha do PKCS#12 (protegida por KMS at-rest)
}

// openVault decodifica o envelope e devolve o parsedPFX pronto pra assinar.
// Centraliza pra Sign e o KMSBackedSigner reusar a mesma lógica.
func (uc *UseCase) openVault(ctx context.Context, cert *Certificate) (*parsedPFX, error) {
	blob, err := uc.cipher.Open(ctx, &cert.Envelope)
	if err != nil {
		return nil, err
	}
	var v vaultBlob
	if err := json.Unmarshal(blob, &v); err != nil {
		return nil, err
	}
	pfx, err := base64.StdEncoding.DecodeString(v.PFXBase64)
	if err != nil {
		return nil, err
	}
	return parsePFX(pfx, v.Password)
}

// Sign assina um digest SHA-256 com a chave do certificado. Fluxo: busca cert
// (metadata + envelope); openVault (KMS.Decrypt → JSON → parsePFX com senha
// armazenada); assina digest; grava o audit row (signing_event) numa tx própria,
// só depois de assinar com sucesso. O parâmetro `password` fica aqui por compat
// com o handler antigo — quando presente, é IGNORADO (senha vem do vault).
func (uc *UseCase) Sign(ctx context.Context, tenantID, id, signerUserID, _ string, digest []byte) (*SignResult, error) {
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
	p, err := uc.openVault(ctx, cert)
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

	err = uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		return uc.repo.RecordSigning(ctx, tx, tenantID, id, signerUserID, digest)
	})
	if err != nil {
		return nil, err
	}
	return &SignResult{Signature: sig, Chain: chain}, nil
}

// SignerInfo carrega metadados legíveis do titular do certificado, além do
// que sai do x509 leaf. Serve pra montar bloco de assinatura no PDF sem
// query extra (OAB fica na nossa tabela, não é parte parseável do cert).
type SignerInfo struct {
	OAB       string // "347019/SP" — como salvo em certificate.oab
	SubjectCN string // "CARLOS TELES TESTE" — redundante com leaf.Subject.CommonName mas explícito
}

// NewSigner devolve um crypto.Signer que assina digests SHA-256 usando este
// certificado (KMS-backed). Usado por libs externas como digitorus/pdfsign
// que exigem crypto.Signer pra montar a assinatura CMS/PAdES.
//
// A leaf certificate + chain acompanham o retorno pra o chamador passar pra
// pdfsign junto com o signer. O SignerInfo carrega OAB (que só está na nossa
// tabela) pra o chamador montar bloco de assinatura no PDF sem query extra.
func (uc *UseCase) NewSigner(ctx context.Context, tenantID, id string) (crypto.Signer, *x509.Certificate, []*x509.Certificate, SignerInfo, error) {
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
		return nil, nil, nil, SignerInfo{}, err
	}
	p, err := uc.openVault(ctx, cert)
	if err != nil {
		return nil, nil, nil, SignerInfo{}, err
	}
	var intermediates []*x509.Certificate
	if len(p.Chain) > 1 {
		intermediates = p.Chain[1:]
	}
	rsaKey, ok := p.Key.(*rsa.PrivateKey)
	if !ok {
		return nil, nil, nil, SignerInfo{}, ErrPKCS12Parse
	}
	info := SignerInfo{OAB: cert.OAB, SubjectCN: cert.SubjectCN}
	return rsaKey, p.Leaf, intermediates, info, nil
}
