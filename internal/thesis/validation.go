package thesis

import (
	"github.com/go-ozzo/ozzo-validation/v4"
)

type CreateThesisRequest struct {
	DraftID         string                `json:"draft_id"`
	PieceProfileKey string                `json:"piece_profile_key,omitempty"`
	NotificationID  string                `json:"notification_id,omitempty"`
	Enunciado       string                `json:"enunciado"`
	Forca           string                `json:"forca"`
	Anchors         []CreateAnchorRequest `json:"anchors"`
}

func (r CreateThesisRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.DraftID, validation.Required),
		validation.Field(&r.Enunciado, validation.Required),
		validation.Field(&r.Forca, validation.Required, validation.In(ForcaFavoravel, ForcaContrariaRelevante)),
		validation.Field(&r.Anchors),
	)
}

type CreateAnchorRequest struct {
	Tipo          string `json:"tipo"`
	AlvoDocumento string `json:"alvo_documento,omitempty"`
	AlvoFonte     string `json:"alvo_fonte,omitempty"`
	Motivo        string `json:"motivo"`
}

func (r CreateAnchorRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Tipo, validation.Required, validation.In(AnchorTipoFato, AnchorTipoDireito)),
		validation.Field(&r.Motivo, validation.Required),
	)
}

type CreateSegmentRequest struct {
	DraftID          string `json:"draft_id"`
	ThesisID         string `json:"thesis_id"`
	ProfileSectionID string `json:"profile_section_id,omitempty"`
	Conteudo         string `json:"conteudo"`
}

func (r CreateSegmentRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.DraftID, validation.Required),
		validation.Field(&r.ThesisID, validation.Required),
		validation.Field(&r.Conteudo, validation.Required),
	)
}
