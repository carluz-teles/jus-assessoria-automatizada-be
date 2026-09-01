package compliancerule

import (
	"github.com/go-ozzo/ozzo-validation/v4"
)

type CreateRuleRequest struct {
	Key         string `json:"key"`
	Descricao   string `json:"descricao"`
	Severidade  string `json:"severidade"`
	FonteLegal  string `json:"fonte_legal,omitempty"`
	Verificacao string `json:"verificacao"`
}

func (r CreateRuleRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Key, validation.Required),
		validation.Field(&r.Descricao, validation.Required),
		validation.Field(&r.Severidade, validation.Required, validation.In(string(SeveridadeBloqueante), string(SeveridadeAviso), string(SeveridadeFeedback))),
		validation.Field(&r.Verificacao, validation.Required, validation.In(string(VerificacaoPorIAAncorada), string(VerificacaoDeterministica), string(VerificacaoFeedbackUsuario))),
	)
}

type UpdateRuleRequest struct {
	Descricao   string `json:"descricao,omitempty"`
	Severidade  string `json:"severidade,omitempty"`
	FonteLegal  string `json:"fonte_legal,omitempty"`
	Verificacao string `json:"verificacao,omitempty"`
}

func (r UpdateRuleRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Severidade, validation.In(string(SeveridadeBloqueante), string(SeveridadeAviso), string(SeveridadeFeedback))),
		validation.Field(&r.Verificacao, validation.In(string(VerificacaoPorIAAncorada), string(VerificacaoDeterministica), string(VerificacaoFeedbackUsuario))),
	)
}

type AddRuleToProfileRequest struct {
	RuleKey            string `json:"rule_key"`
	OverrideSeveridade string `json:"override_severidade,omitempty"`
}

func (r AddRuleToProfileRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.RuleKey, validation.Required),
		validation.Field(&r.OverrideSeveridade, validation.In("", string(SeveridadeBloqueante), string(SeveridadeAviso), string(SeveridadeFeedback))),
	)
}
