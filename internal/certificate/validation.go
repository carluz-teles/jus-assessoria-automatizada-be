package certificate

import (
	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/go-ozzo/ozzo-validation/v4/is"
)

// SignRequest é o body JSON de POST /v1/certificates/:id/sign — senha da
// sessão (obrigatória; nunca persistida) e o digest base64 SHA-256 do documento
// a assinar (32 bytes decodificados). Contrato bate 1:1 com o FE.
type SignRequest struct {
	Password      string `json:"password"`
	DigestSHA256B64 string `json:"digest_sha256"`
}

func (r SignRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Password, validation.Required),
		validation.Field(&r.DigestSHA256B64,
			validation.Required,
			is.Base64,
		),
	)
}
