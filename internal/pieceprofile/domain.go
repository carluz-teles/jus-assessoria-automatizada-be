package pieceprofile

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// Repository is the persistence port for the piece_profile catalog (global — no
// tenant scoping, docs/erd-tipos-de-peca.md §7.1). tenantID is threaded through
// every method for signature consistency with the rest of the platform's slices
// (CLAUDE.md: tenantID in every repo signature), even though it is not used to
// filter these global catalog tables.
type Repository interface {
	GetProfileByKey(ctx context.Context, tx database.Tx, tenantID, key string) (*PieceProfile, error)
	ListProfiles(ctx context.Context, tx database.Tx, tenantID, matterKey string) ([]PieceProfile, error)
	InsertProfile(ctx context.Context, tx database.Tx, tenantID string, p *PieceProfile) (*PieceProfile, error)
	UpdateProfile(ctx context.Context, tx database.Tx, tenantID, key string, p *PieceProfile) (*PieceProfile, error)

	GetSectionsByProfile(ctx context.Context, tx database.Tx, tenantID, profileKey string) ([]ProfileSection, error)
	InsertSection(ctx context.Context, tx database.Tx, tenantID string, s *ProfileSection) (*ProfileSection, error)
	UpdateSection(ctx context.Context, tx database.Tx, tenantID, sectionID string, s *ProfileSection) (*ProfileSection, error)
	DeleteSection(ctx context.Context, tx database.Tx, tenantID, sectionID string) error

	GetRequirementsByProfile(ctx context.Context, tx database.Tx, tenantID, profileKey string) ([]ProfileRequirement, error)
	InsertRequirement(ctx context.Context, tx database.Tx, tenantID string, r *ProfileRequirement) (*ProfileRequirement, error)

	InsertVersion(ctx context.Context, tx database.Tx, tenantID string, v *PieceProfileVersion) (*PieceProfileVersion, error)
	GetVersionByKeyAndVersion(ctx context.Context, tx database.Tx, tenantID, profileKey, version string) (*PieceProfileVersion, error)

	GetMatterByKey(ctx context.Context, tx database.Tx, key string) (*Matter, error)
	ListMatters(ctx context.Context, tx database.Tx) ([]Matter, error)
	GetBaseSkeletonByKey(ctx context.Context, tx database.Tx, key string) (*BaseSkeleton, error)
	ListBaseSkeletons(ctx context.Context, tx database.Tx) ([]BaseSkeleton, error)
	GetFormatProfileByKey(ctx context.Context, tx database.Tx, key string) (*FormatProfile, error)
	ListFormatProfiles(ctx context.Context, tx database.Tx) ([]FormatProfile, error)
}

// OutboxPublisher is the transactional-outbox producer port (see lib/events.Outbox).
type OutboxPublisher interface {
	Publish(ctx context.Context, tx database.Tx, ev events.Event) error
}

type UseCase struct {
	repo   database.UnitOfWork
	rw     Repository
	now    func() time.Time
	outbox OutboxPublisher
}

func NewUseCase(uow database.UnitOfWork, repo Repository, opts ...Option) *UseCase {
	uc := &UseCase{repo: uow, rw: repo, now: time.Now}
	for _, opt := range opts {
		opt(uc)
	}
	return uc
}

type Option func(*UseCase)

func WithClock(now func() time.Time) Option {
	return func(uc *UseCase) { uc.now = now }
}

func WithOutbox(ob OutboxPublisher) Option {
	return func(uc *UseCase) { uc.outbox = ob }
}

type CreateProfileCommand struct {
	TenantID         string
	Key              string
	Nome             string
	Polo             string
	MatterKey        string
	BaseSkeletonKey  string
	FormatProfileKey string
	FonteLegal       []byte
}

func (uc *UseCase) CreateProfile(ctx context.Context, cmd CreateProfileCommand) (*PieceProfile, error) {
	var result *PieceProfile

	err := uc.repo.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		if _, err := uc.rw.GetMatterByKey(ctx, tx, cmd.MatterKey); err != nil {
			return err
		}
		if _, err := uc.rw.GetBaseSkeletonByKey(ctx, tx, cmd.BaseSkeletonKey); err != nil {
			return err
		}
		if cmd.FormatProfileKey != "" {
			if _, err := uc.rw.GetFormatProfileByKey(ctx, tx, cmd.FormatProfileKey); err != nil {
				return err
			}
		}

		p := &PieceProfile{
			Key:              cmd.Key,
			Nome:             cmd.Nome,
			Polo:             cmd.Polo,
			MatterKey:        cmd.MatterKey,
			BaseSkeletonKey:  cmd.BaseSkeletonKey,
			FormatProfileKey: cmd.FormatProfileKey,
			VersionAtual:     "v1",
			FonteLegal:       cmd.FonteLegal,
		}

		created, err := uc.rw.InsertProfile(ctx, tx, cmd.TenantID, p)
		if err != nil {
			return err
		}

		if uc.outbox != nil {
			ev := newPieceProfileCreated(created, cmd.TenantID)
			if err := uc.outbox.Publish(ctx, tx, ev); err != nil {
				return err
			}
		}

		result = created
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

type UpdateProfileCommand struct {
	TenantID         string
	Key              string
	Nome             *string
	Polo             *string
	MatterKey        *string
	BaseSkeletonKey  *string
	FormatProfileKey *string
	FonteLegal       []byte
}

func (uc *UseCase) UpdateProfile(ctx context.Context, cmd UpdateProfileCommand) (*PieceProfile, error) {
	var result *PieceProfile

	err := uc.repo.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		existing, err := uc.rw.GetProfileByKey(ctx, tx, cmd.TenantID, cmd.Key)
		if err != nil {
			return err
		}

		if cmd.Nome != nil {
			existing.Nome = *cmd.Nome
		}
		if cmd.Polo != nil {
			existing.Polo = *cmd.Polo
		}
		if cmd.MatterKey != nil {
			if _, err := uc.rw.GetMatterByKey(ctx, tx, *cmd.MatterKey); err != nil {
				return err
			}
			existing.MatterKey = *cmd.MatterKey
		}
		if cmd.BaseSkeletonKey != nil {
			if _, err := uc.rw.GetBaseSkeletonByKey(ctx, tx, *cmd.BaseSkeletonKey); err != nil {
				return err
			}
			existing.BaseSkeletonKey = *cmd.BaseSkeletonKey
		}
		if cmd.FormatProfileKey != nil {
			if _, err := uc.rw.GetFormatProfileByKey(ctx, tx, *cmd.FormatProfileKey); err != nil {
				return err
			}
			existing.FormatProfileKey = *cmd.FormatProfileKey
		}
		if cmd.FonteLegal != nil {
			existing.FonteLegal = cmd.FonteLegal
		}

		updated, err := uc.rw.UpdateProfile(ctx, tx, cmd.TenantID, cmd.Key, existing)
		if err != nil {
			return err
		}

		if uc.outbox != nil {
			ev := newPieceProfileUpdated(updated, cmd.TenantID)
			if err := uc.outbox.Publish(ctx, tx, ev); err != nil {
				return err
			}
		}

		result = updated
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (uc *UseCase) GetProfile(ctx context.Context, tenantID, key string) (*PieceProfile, error) {
	var result *PieceProfile

	err := uc.repo.Do(ctx, tenantID, func(tx database.Tx) error {
		p, err := uc.rw.GetProfileByKey(ctx, tx, tenantID, key)
		if err != nil {
			return err
		}

		sections, err := uc.rw.GetSectionsByProfile(ctx, tx, tenantID, key)
		if err != nil {
			return err
		}
		p.Sections = sections

		requirements, err := uc.rw.GetRequirementsByProfile(ctx, tx, tenantID, key)
		if err != nil {
			return err
		}
		p.Requirements = requirements

		result = p
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (uc *UseCase) ListProfiles(ctx context.Context, tenantID, matterKey string) ([]PieceProfile, error) {
	var result []PieceProfile

	err := uc.repo.Do(ctx, tenantID, func(tx database.Tx) error {
		profiles, err := uc.rw.ListProfiles(ctx, tx, tenantID, matterKey)
		if err != nil {
			return err
		}
		result = profiles
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

type CreateSectionCommand struct {
	TenantID    string
	ProfileKey  string
	Key         string
	Titulo      string
	Ordem       int
	Obrigatoria string
	Origem      string
	AceitaTeses bool
	FonteLegal  []byte
}

func (uc *UseCase) CreateSection(ctx context.Context, cmd CreateSectionCommand) (*ProfileSection, error) {
	var result *ProfileSection

	err := uc.repo.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		if _, err := uc.rw.GetProfileByKey(ctx, tx, cmd.TenantID, cmd.ProfileKey); err != nil {
			return err
		}

		s := &ProfileSection{
			PieceProfileKey: cmd.ProfileKey,
			Key:             cmd.Key,
			Titulo:          cmd.Titulo,
			Ordem:           cmd.Ordem,
			Obrigatoria:     cmd.Obrigatoria,
			Origem:          cmd.Origem,
			AceitaTeses:     cmd.AceitaTeses,
			FonteLegal:      cmd.FonteLegal,
		}

		created, err := uc.rw.InsertSection(ctx, tx, cmd.TenantID, s)
		if err != nil {
			return err
		}
		result = created
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

type UpdateSectionCommand struct {
	TenantID    string
	SectionID   string
	Key         *string
	Titulo      *string
	Ordem       *int
	Obrigatoria *string
	Origem      *string
	AceitaTeses *bool
	FonteLegal  []byte
}

func (uc *UseCase) UpdateSection(ctx context.Context, cmd UpdateSectionCommand) (*ProfileSection, error) {
	var result *ProfileSection

	err := uc.repo.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		s := &ProfileSection{}
		if cmd.Key != nil {
			s.Key = *cmd.Key
		}
		if cmd.Titulo != nil {
			s.Titulo = *cmd.Titulo
		}
		if cmd.Ordem != nil {
			s.Ordem = *cmd.Ordem
		}
		if cmd.Obrigatoria != nil {
			s.Obrigatoria = *cmd.Obrigatoria
		}
		if cmd.Origem != nil {
			s.Origem = *cmd.Origem
		}
		if cmd.AceitaTeses != nil {
			s.AceitaTeses = *cmd.AceitaTeses
		}
		if cmd.FonteLegal != nil {
			s.FonteLegal = cmd.FonteLegal
		}

		updated, err := uc.rw.UpdateSection(ctx, tx, cmd.TenantID, cmd.SectionID, s)
		if err != nil {
			return err
		}
		result = updated
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (uc *UseCase) DeleteSection(ctx context.Context, tenantID, sectionID string) error {
	return uc.repo.Do(ctx, tenantID, func(tx database.Tx) error {
		return uc.rw.DeleteSection(ctx, tx, tenantID, sectionID)
	})
}

func (uc *UseCase) ListRequirements(ctx context.Context, tenantID, profileKey string) ([]ProfileRequirement, error) {
	var result []ProfileRequirement

	err := uc.repo.Do(ctx, tenantID, func(tx database.Tx) error {
		reqs, err := uc.rw.GetRequirementsByProfile(ctx, tx, tenantID, profileKey)
		if err != nil {
			return err
		}
		result = reqs
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// CreateVersionCommand snapshots a piece_profile's current sections + requirements
// under an EXPLICIT version label supplied by the caller. version_atual is a free
// string (v1, v1.1, 2025-09-01, ...), not a counter — the caller decides the next
// label; it is never derived by incrementing a prior value.
type CreateVersionCommand struct {
	TenantID   string
	ProfileKey string
	Version    string
}

func (uc *UseCase) CreateVersion(ctx context.Context, cmd CreateVersionCommand) (*PieceProfileVersion, error) {
	if cmd.Version == "" {
		return nil, apperr.NewInvalid("version is required")
	}

	var result *PieceProfileVersion

	err := uc.repo.Do(ctx, cmd.TenantID, func(tx database.Tx) error {
		p, err := uc.rw.GetProfileByKey(ctx, tx, cmd.TenantID, cmd.ProfileKey)
		if err != nil {
			return err
		}

		sections, err := uc.rw.GetSectionsByProfile(ctx, tx, cmd.TenantID, cmd.ProfileKey)
		if err != nil {
			return err
		}
		requirements, err := uc.rw.GetRequirementsByProfile(ctx, tx, cmd.TenantID, cmd.ProfileKey)
		if err != nil {
			return err
		}

		p.Sections = sections
		p.Requirements = requirements

		snapshot, err := json.Marshal(p)
		if err != nil {
			return database.WrapInfra(err)
		}

		v := &PieceProfileVersion{
			PieceProfileKey: cmd.ProfileKey,
			Version:         cmd.Version,
			VigenteDesde:    uc.now(),
			Snapshot:        snapshot,
		}

		created, err := uc.rw.InsertVersion(ctx, tx, cmd.TenantID, v)
		if err != nil {
			return err
		}

		if uc.outbox != nil {
			ev := newPieceProfileVersionCreated(created, cmd.TenantID)
			if err := uc.outbox.Publish(ctx, tx, ev); err != nil {
				return err
			}
		}

		result = created
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (uc *UseCase) GetVersion(ctx context.Context, tenantID, profileKey, version string) (*PieceProfileVersion, error) {
	var result *PieceProfileVersion

	err := uc.repo.Do(ctx, tenantID, func(tx database.Tx) error {
		v, err := uc.rw.GetVersionByKeyAndVersion(ctx, tx, tenantID, profileKey, version)
		if err != nil {
			return err
		}
		result = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (uc *UseCase) GetMatter(ctx context.Context, key string) (*Matter, error) {
	var result *Matter
	err := uc.repo.Do(ctx, "", func(tx database.Tx) error {
		m, err := uc.rw.GetMatterByKey(ctx, tx, key)
		if err != nil {
			return err
		}
		result = m
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (uc *UseCase) ListMatters(ctx context.Context) ([]Matter, error) {
	var result []Matter
	err := uc.repo.Do(ctx, "", func(tx database.Tx) error {
		matters, err := uc.rw.ListMatters(ctx, tx)
		if err != nil {
			return err
		}
		result = matters
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (uc *UseCase) GetBaseSkeleton(ctx context.Context, key string) (*BaseSkeleton, error) {
	var result *BaseSkeleton
	err := uc.repo.Do(ctx, "", func(tx database.Tx) error {
		bs, err := uc.rw.GetBaseSkeletonByKey(ctx, tx, key)
		if err != nil {
			return err
		}
		result = bs
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (uc *UseCase) ListBaseSkeletons(ctx context.Context) ([]BaseSkeleton, error) {
	var result []BaseSkeleton
	err := uc.repo.Do(ctx, "", func(tx database.Tx) error {
		skeletons, err := uc.rw.ListBaseSkeletons(ctx, tx)
		if err != nil {
			return err
		}
		result = skeletons
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (uc *UseCase) GetFormatProfile(ctx context.Context, key string) (*FormatProfile, error) {
	var result *FormatProfile
	err := uc.repo.Do(ctx, "", func(tx database.Tx) error {
		fp, err := uc.rw.GetFormatProfileByKey(ctx, tx, key)
		if err != nil {
			return err
		}
		result = fp
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (uc *UseCase) ListFormatProfiles(ctx context.Context) ([]FormatProfile, error) {
	var result []FormatProfile
	err := uc.repo.Do(ctx, "", func(tx database.Tx) error {
		profiles, err := uc.rw.ListFormatProfiles(ctx, tx)
		if err != nil {
			return err
		}
		result = profiles
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
