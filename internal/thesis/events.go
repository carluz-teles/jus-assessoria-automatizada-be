package thesis

import (
	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/events"
)

const aggregateTypeThesis = "thesis"

const TypeThesisCreated = "thesis.created"
const TypeThesisApproved = "thesis.approved"
const TypeThesisDiscarded = "thesis.discarded"
const TypeThesisCoverageChecked = "thesis.coverage_checked"

type ThesisCreated struct {
	events.Base
	ThesisID  string `json:"thesis_id"`
	TenantID  string `json:"tenant_id"`
	DraftID   string `json:"draft_id"`
	Enunciado string `json:"enunciado"`
	Forca     string `json:"forca"`
}

var _ events.Event = ThesisCreated{}

func (ThesisCreated) Type() string          { return TypeThesisCreated }
func (ThesisCreated) AggregateType() string { return aggregateTypeThesis }

func newThesisCreated(t *Thesis) ThesisCreated {
	return ThesisCreated{
		Base:      events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: t.ID},
		ThesisID:  t.ID,
		TenantID:  t.TenantID,
		DraftID:   t.DraftID,
		Enunciado: t.Enunciado,
		Forca:     t.Forca,
	}
}

type ThesisApproved struct {
	events.Base
	ThesisID string `json:"thesis_id"`
	TenantID string `json:"tenant_id"`
}

var _ events.Event = ThesisApproved{}

func (ThesisApproved) Type() string          { return TypeThesisApproved }
func (ThesisApproved) AggregateType() string { return aggregateTypeThesis }

func newThesisApproved(t *Thesis) ThesisApproved {
	return ThesisApproved{
		Base:     events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: t.ID},
		ThesisID: t.ID,
		TenantID: t.TenantID,
	}
}

type ThesisDiscarded struct {
	events.Base
	ThesisID string `json:"thesis_id"`
	TenantID string `json:"tenant_id"`
}

var _ events.Event = ThesisDiscarded{}

func (ThesisDiscarded) Type() string          { return TypeThesisDiscarded }
func (ThesisDiscarded) AggregateType() string { return aggregateTypeThesis }

func newThesisDiscarded(t *Thesis) ThesisDiscarded {
	return ThesisDiscarded{
		Base:     events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: t.ID},
		ThesisID: t.ID,
		TenantID: t.TenantID,
	}
}

type ThesisCoverageChecked struct {
	events.Base
	TenantID  string `json:"tenant_id"`
	DraftID   string `json:"draft_id"`
	ThesisID  string `json:"thesis_id"`
	Resultado string `json:"resultado"`
}

var _ events.Event = ThesisCoverageChecked{}

func (ThesisCoverageChecked) Type() string          { return TypeThesisCoverageChecked }
func (ThesisCoverageChecked) AggregateType() string { return aggregateTypeThesis }

func newThesisCoverageChecked(tenantID, draftID string, c *ThesisCoverage) ThesisCoverageChecked {
	return ThesisCoverageChecked{
		Base:      events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: c.ID},
		TenantID:  tenantID,
		DraftID:   draftID,
		ThesisID:  c.ThesisID,
		Resultado: c.Resultado,
	}
}
