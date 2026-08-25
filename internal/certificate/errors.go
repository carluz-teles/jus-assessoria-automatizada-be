package certificate

import "github.com/jusassessoria/platform/lib/apperr"

// Sentinel errors — kept typed via apperr so the httpx boundary maps them to
// HTTP status without the domain caring about the wire format.

// ErrCertificateNotFound: no active certificate for (tenant, id).
var ErrCertificateNotFound = apperr.NewNotFound("certificado não encontrado")

// ErrPKCS12BadPassword: password could not open the .pfx. This is a user
// input error, not infra — the FE surfaces it as a form validation.
var ErrPKCS12BadPassword = apperr.NewInvalid("senha do certificado incorreta")

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
