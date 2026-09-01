package pieceprofile

import (
	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/events"
)

const aggregateTypePieceProfile = "piece_profile"

const TypePieceProfileCreated = "piece_profile.created"

type PieceProfileCreated struct {
	events.Base
	ProfileKey string `json:"profile_key"`
	TenantID   string `json:"tenant_id"`
}

var _ events.Event = PieceProfileCreated{}

func (PieceProfileCreated) Type() string          { return TypePieceProfileCreated }
func (PieceProfileCreated) AggregateType() string { return aggregateTypePieceProfile }

func newPieceProfileCreated(p *PieceProfile, tenantID string) PieceProfileCreated {
	return PieceProfileCreated{
		Base:       events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: p.Key},
		ProfileKey: p.Key,
		TenantID:   tenantID,
	}
}

const TypePieceProfileUpdated = "piece_profile.updated"

type PieceProfileUpdated struct {
	events.Base
	ProfileKey string `json:"profile_key"`
	TenantID   string `json:"tenant_id"`
}

var _ events.Event = PieceProfileUpdated{}

func (PieceProfileUpdated) Type() string          { return TypePieceProfileUpdated }
func (PieceProfileUpdated) AggregateType() string { return aggregateTypePieceProfile }

func newPieceProfileUpdated(p *PieceProfile, tenantID string) PieceProfileUpdated {
	return PieceProfileUpdated{
		Base:       events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: p.Key},
		ProfileKey: p.Key,
		TenantID:   tenantID,
	}
}

const TypePieceProfileVersionCreated = "piece_profile.version_created"

type PieceProfileVersionCreated struct {
	events.Base
	ProfileKey string `json:"profile_key"`
	Version    string `json:"version"`
	TenantID   string `json:"tenant_id"`
}

var _ events.Event = PieceProfileVersionCreated{}

func (PieceProfileVersionCreated) Type() string          { return TypePieceProfileVersionCreated }
func (PieceProfileVersionCreated) AggregateType() string { return aggregateTypePieceProfile }

func newPieceProfileVersionCreated(v *PieceProfileVersion, tenantID string) PieceProfileVersionCreated {
	return PieceProfileVersionCreated{
		Base:       events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: v.PieceProfileKey},
		ProfileKey: v.PieceProfileKey,
		Version:    v.Version,
		TenantID:   tenantID,
	}
}
