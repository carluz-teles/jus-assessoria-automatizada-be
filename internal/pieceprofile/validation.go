package pieceprofile

import (
	"errors"

	validation "github.com/go-ozzo/ozzo-validation/v4"
)

type CreateProfileRequest struct {
	Key              string `json:"key"`
	Nome             string `json:"nome"`
	Polo             string `json:"polo"`
	MatterKey        string `json:"matter_key"`
	BaseSkeletonKey  string `json:"base_skeleton_key"`
	FormatProfileKey string `json:"format_profile_key,omitempty"`
	FonteLegal       []byte `json:"fonte_legal,omitempty"`
}

func (r CreateProfileRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Key, validation.Required, validation.Length(1, 128)),
		validation.Field(&r.Nome, validation.Required, validation.Length(1, 255)),
		validation.Field(&r.Polo, validation.Required, validation.By(isValidPolo)),
		validation.Field(&r.MatterKey, validation.Required),
		validation.Field(&r.BaseSkeletonKey, validation.Required),
	)
}

type UpdateProfileRequest struct {
	Nome             *string `json:"nome"`
	Polo             *string `json:"polo"`
	MatterKey        *string `json:"matter_key"`
	BaseSkeletonKey  *string `json:"base_skeleton_key"`
	FormatProfileKey *string `json:"format_profile_key"`
	FonteLegal       []byte  `json:"fonte_legal"`
}

func (r UpdateProfileRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Nome,
			validation.When(r.Nome != nil, validation.Required, validation.Length(1, 255)),
		),
		validation.Field(&r.Polo,
			validation.When(r.Polo != nil, validation.Required, validation.By(isValidPolo)),
		),
		validation.Field(&r.MatterKey,
			validation.When(r.MatterKey != nil, validation.Required),
		),
		validation.Field(&r.BaseSkeletonKey,
			validation.When(r.BaseSkeletonKey != nil, validation.Required),
		),
	)
}

type CreateSectionRequest struct {
	Key         string `json:"key"`
	Titulo      string `json:"titulo"`
	Ordem       int    `json:"ordem"`
	Obrigatoria string `json:"obrigatoria"`
	Origem      string `json:"origem"`
	AceitaTeses bool   `json:"aceita_teses"`
	FonteLegal  []byte `json:"fonte_legal"`
}

func (r CreateSectionRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Key, validation.Required, validation.Length(1, 128)),
		validation.Field(&r.Titulo, validation.Required, validation.Length(1, 255)),
		validation.Field(&r.Ordem, validation.Required),
		validation.Field(&r.Obrigatoria, validation.Required, validation.By(isValidObrigatoria)),
		validation.Field(&r.Origem, validation.Required, validation.By(isValidOrigem)),
	)
}

type UpdateSectionRequest struct {
	Key         *string `json:"key"`
	Titulo      *string `json:"titulo"`
	Ordem       *int    `json:"ordem"`
	Obrigatoria *string `json:"obrigatoria"`
	Origem      *string `json:"origem"`
	AceitaTeses *bool   `json:"aceita_teses"`
	FonteLegal  []byte  `json:"fonte_legal"`
}

func (r UpdateSectionRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Key,
			validation.When(r.Key != nil, validation.Required, validation.Length(1, 128)),
		),
		validation.Field(&r.Titulo,
			validation.When(r.Titulo != nil, validation.Required, validation.Length(1, 255)),
		),
		validation.Field(&r.Obrigatoria,
			validation.When(r.Obrigatoria != nil, validation.Required, validation.By(isValidObrigatoria)),
		),
		validation.Field(&r.Origem,
			validation.When(r.Origem != nil, validation.Required, validation.By(isValidOrigem)),
		),
	)
}

// CreateVersionRequest carries the EXPLICIT version label for a new snapshot
// (docs/erd-tipos-de-peca.md §2 — version_atual is free text, not a counter).
type CreateVersionRequest struct {
	Version string `json:"version"`
}

func (r CreateVersionRequest) Validate() error {
	return validation.ValidateStruct(&r,
		validation.Field(&r.Version, validation.Required, validation.Length(1, 64)),
	)
}

func isValidPolo(value any) error {
	s, _ := value.(string)
	if !validPolos[s] {
		return errors.New("must be ativo, passivo, or ambos")
	}
	return nil
}

func isValidObrigatoria(value any) error {
	s, _ := value.(string)
	if !validObrigatorias[s] {
		return errors.New("must be sim, nao, or condicional")
	}
	return nil
}

func isValidOrigem(value any) error {
	s, _ := value.(string)
	if !validOrigens[s] {
		return errors.New("must be moldura or argumentativa")
	}
	return nil
}
