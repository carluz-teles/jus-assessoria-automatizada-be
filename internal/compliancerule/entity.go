package compliancerule

type Severidade string

const (
	SeveridadeBloqueante Severidade = "bloqueante"
	SeveridadeAviso      Severidade = "aviso"
	SeveridadeFeedback   Severidade = "feedback"
)

type Verificacao string

const (
	VerificacaoPorIAAncorada   Verificacao = "por_ia_ancorada"
	VerificacaoDeterministica  Verificacao = "deterministica"
	VerificacaoFeedbackUsuario Verificacao = "feedback_usuario"
)

// ComplianceRule is a global catalog row (docs/erd-tipos-de-peca.md §2) — no
// created_at/updated_at columns on compliance_rule (migration 0077): it is seed
// data (cadastro), not an audited per-tenant record, so the entity carries no
// audit timestamps either.
type ComplianceRule struct {
	Key         string
	Descricao   string
	Severidade  Severidade
	FonteLegal  string
	Verificacao Verificacao
}

type ProfileRule struct {
	ID                 string
	PieceProfileKey    string
	ComplianceRuleKey  string
	OverrideSeveridade Severidade
}

type SectionRule struct {
	ID                string
	ProfileSectionID  string
	ComplianceRuleKey string
}
