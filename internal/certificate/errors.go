package certificate

import (
	"strings"

	"github.com/jusassessoria/platform/lib/apperr"
)

// Sentinel errors — kept typed via apperr so the httpx boundary maps them to
// HTTP status without the domain caring about the wire format.

// ErrCertificateNotFound: no active certificate for (tenant, id).
var ErrCertificateNotFound = apperr.NewNotFound("certificado não encontrado")

// ErrPKCS12BadPassword: password could not open the .pfx. This is a user
// input error, not infra — the FE surfaces it as a form validation.
var ErrPKCS12BadPassword = apperr.NewInvalid("senha do certificado incorreta")

// ErrInvalidPassword: password could not decrypt the .pfx during signing. Alias
// of ErrPKCS12BadPassword kept for semantic clarity in the signing path.
var ErrInvalidPassword = ErrPKCS12BadPassword

// ErrPKCS12Parse: the .pfx is unreadable (corrupt, wrong format). Distinguished
// from BadPassword so the FE can advise "arquivo inválido" vs "senha incorreta".
var ErrPKCS12Parse = apperr.NewInvalid("arquivo do certificado inválido")

// ErrCertificateAlreadyExists: dedup — mesmo fingerprint já cadastrado ativo
// para o tenant. O FE mostra "este certificado já está cadastrado".
var ErrCertificateAlreadyExists = apperr.NewConflict("este certificado já está cadastrado")

// ErrCertificateExpired: NotAfter no passado — bloqueia upload (não faz
// sentido cadastrar um cert que não pode assinar nada).
var ErrCertificateExpired = apperr.NewInvalid("certificado expirado")

// ErrStorageNotConfigured: o slice sobe mas o storage.Client é nil (dev sem
// MinIO ou prod sem R2). O handler responde 503 pra o FE mostrar "serviço
// indisponível" em vez de crashar.
var ErrStorageNotConfigured = apperr.NewInfra("storage de certificados não configurado", nil)

// ErrInvalidDigest: o digest não tem exatamente 32 bytes (SHA-256). Rejeitado
// antes de decriptar o cert pra nunca assinar dados de tamanho atacante.
var ErrInvalidDigest = apperr.NewInvalid("digest deve ter exatamente 32 bytes (SHA-256)")

// ErrMalformedPFX: o .pfx foi decriptado mas não contém um leaf certificate
// válido (cadeia vazia ou estrutura PKCS#12 corrompida).
var ErrMalformedPFX = apperr.NewInvalid("certificado PKCS#12 malformado (sem leaf)")

// classifyPKCS12Error inspects a pkcs12.DecodeChain error and returns the
// appropriate typed error (bad password vs malformed file).
func classifyPKCS12Error(err error) error {
	low := strings.ToLower(err.Error())
	if strings.Contains(low, "password") || strings.Contains(low, "mac verify") {
		return ErrInvalidPassword
	}
	return ErrPKCS12Parse
}
