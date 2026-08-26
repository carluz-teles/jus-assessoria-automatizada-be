package certificate

import (
	validation "github.com/go-ozzo/ozzo-validation/v4"
	"github.com/go-ozzo/ozzo-validation/v4/is"
)

// SignRequest é o body JSON de POST /v1/certificates/:id/sign — senha da
// sessão (obrigatória; nunca persistida) e o digest base64 SHA-256 do documento
// a assinar (32 bytes decodificados). Contrato bate 1:1 com o FE.
type SignRequest struct {
	Password        string `json:"password"`
	DigestSHA256B64 string `json:"digest_sha256"`
}

func (r SignRequest) Validate() error {
	return validation.ValidateStruct(&r,
		// Password NÃO é validation.Required: sua obrigatoriedade agora é regra
		// de domínio dependente de PasswordPolicy (ver Sign em domain.go), não
		// de shape do request — um certificado com policy="never" assina sem
		// senha no body.
		validation.Field(&r.DigestSHA256B64,
			validation.Required,
			is.Base64,
		),
	)
}

// UpdatePasswordPolicyRequest é o body JSON de
// PATCH /v1/certificates/:id/password-policy.
type UpdatePasswordPolicyRequest struct {
	PasswordPolicy string `json:"password_policy"`
}

// Validate checa só a forma (campo presente); o enum em si
// ('always'/'session'/'never') é responsabilidade de PasswordPolicy.Valid()
// no domínio — uma única fonte de verdade pro conjunto de valores aceitos.
func (r UpdatePasswordPolicyRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.PasswordPolicy, validation.Required),
	)
}
