// Package certificate is the vertical slice for digital certificates (ICP-Brasil
// A1 .pfx/PKCS#12) used by the advogado to sign peças. It stores metadata + a
// server-encrypted copy of the .pfx so the API can re-open and sign on demand,
// but NEVER persists the user's password (it is asked on every sign). See
// docs/erd-pecas.md (§Assinatura) for the product story.
//
// Fatia 1 (this slice) implements CRUD + preview + digest sign. The integration
// with draft.sign (assinar a PDF de verdade) is fatia 2.
package certificate

import "time"

// Certificate is the aggregate root — the metadata of one .pfx cadastrado for a
// tenant/user. O binário .pfx vive in-row cifrado pelo envelope (DEK aleatória
// AES-GCM local + DEK wrapped pelo GCP KMS). Sem KMS master key, sem forma de
// decifrar — nem a service account do api sozinha basta, precisa da KMS role.
type Certificate struct {
	ID          string
	TenantID    string
	OwnerUserID string
	SubjectCN   string    // Common Name do titular ("LUAN GOMES")
	OAB         string    // "347019/SP" quando o cert traz; "" quando não
	Issuer      string    // AC emissora (ex.: "AC Certisign RFB G5")
	Serial      string    // número de série do X.509
	NotBefore   time.Time // início de validade
	NotAfter    time.Time // fim de validade
	Fingerprint string    // SHA-256 do DER do cert, hex sem separador
	Envelope    Envelope  // ciphertext + nonce + wrapped_dek + kek_ref
	CreatedAt   time.Time
	RevokedAt   *time.Time // nil = ativo; non-nil = soft-deleted
}

// CertMetadata is the shape returned by pkcs12 parse (preview or upload). It
// carries every metadata field but NO reference to the binary or the password —
// those live only in the calling context (memory, request body).
type CertMetadata struct {
	SubjectCN   string
	OAB         string
	Issuer      string
	Serial      string
	NotBefore   time.Time
	NotAfter    time.Time
	Fingerprint string
	// CertChainDER is the DER-encoded chain (leaf first), used by the sign
	// response so the FE can embed it in signature envelopes (PAdES/CAdES).
	CertChainDER [][]byte
}

// PreviewResult is what POST /certificates/preview answers: metadata + boolean
// checks the FE renders as ✓/✗ in the wizard's "Validação" step. The .pfx is
// discarded; nothing is persisted.
type PreviewResult struct {
	Meta   CertMetadata
	Checks PreviewChecks
}

// PreviewChecks are the go/no-go signals the wizard shows.
type PreviewChecks struct {
	NaoExpirado    bool // agora ∈ [NotBefore, NotAfter]
	CadeiaOk       bool // o .pfx traz ao menos a leaf + issuer
	TitularConfere bool // SubjectCN não vazio
}

// SignResult is the output of POST /certificates/:id/sign — the signature
// (raw RSA/ECDSA bytes) and the chain used, both base64-encoded on the wire.
type SignResult struct {
	Signature []byte
	Chain     [][]byte // leaf primeiro, DER
}
