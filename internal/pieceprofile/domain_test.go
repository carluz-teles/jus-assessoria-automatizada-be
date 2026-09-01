package pieceprofile

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// fakeUOW is a no-op unit of work: it records the RLS scope the use case asked for
// and runs fn with a nil tx (the mocked repo never touches it).
type fakeUOW struct {
	scope string
	err   error
}

func (u *fakeUOW) Do(ctx context.Context, tenantID string, fn func(tx database.Tx) error) error {
	u.scope = tenantID
	if u.err != nil {
		return u.err
	}
	return fn(nil)
}

func (u *fakeUOW) DoSystem(_ context.Context, fn func(tx database.Tx) error) error {
	if u.err != nil {
		return u.err
	}
	return fn(nil)
}

// recordingOutbox captures what a use case publishes so a test can assert the
// right event is emitted in the same unit of work.
type recordingOutbox struct {
	published []events.Event
	err       error
}

func (r *recordingOutbox) Publish(_ context.Context, _ database.Tx, ev events.Event) error {
	if r.err != nil {
		return r.err
	}
	r.published = append(r.published, ev)
	return nil
}

// mockRepo is a hand-rolled Repository: each method returns a configured value.
type mockRepo struct {
	matter    *Matter
	matterErr error

	baseSkeleton    *BaseSkeleton
	baseSkeletonErr error

	formatProfile    *FormatProfile
	formatProfileErr error

	insertedProfile *PieceProfile
	insertErr       error

	getProfile    *PieceProfile
	getProfileErr error

	sections     []ProfileSection
	sectionsErr  error
	requirements []ProfileRequirement
	reqErr       error

	insertedVersion *PieceProfileVersion
	insertVerErr    error
}

func (m *mockRepo) GetProfileByKey(ctx context.Context, tx database.Tx, tenantID, key string) (*PieceProfile, error) {
	if m.getProfileErr != nil {
		return nil, m.getProfileErr
	}
	return m.getProfile, nil
}

func (m *mockRepo) ListProfiles(ctx context.Context, tx database.Tx, tenantID, matterKey string) ([]PieceProfile, error) {
	return nil, nil
}

func (m *mockRepo) InsertProfile(ctx context.Context, tx database.Tx, tenantID string, p *PieceProfile) (*PieceProfile, error) {
	if m.insertErr != nil {
		return nil, m.insertErr
	}
	if m.insertedProfile != nil {
		return m.insertedProfile, nil
	}
	return p, nil
}

func (m *mockRepo) UpdateProfile(ctx context.Context, tx database.Tx, tenantID, key string, p *PieceProfile) (*PieceProfile, error) {
	return p, nil
}

func (m *mockRepo) GetSectionsByProfile(ctx context.Context, tx database.Tx, tenantID, profileKey string) ([]ProfileSection, error) {
	return m.sections, m.sectionsErr
}

func (m *mockRepo) InsertSection(ctx context.Context, tx database.Tx, tenantID string, s *ProfileSection) (*ProfileSection, error) {
	return s, nil
}

func (m *mockRepo) UpdateSection(ctx context.Context, tx database.Tx, tenantID, sectionID string, s *ProfileSection) (*ProfileSection, error) {
	return s, nil
}

func (m *mockRepo) DeleteSection(ctx context.Context, tx database.Tx, tenantID, sectionID string) error {
	return nil
}

func (m *mockRepo) GetRequirementsByProfile(ctx context.Context, tx database.Tx, tenantID, profileKey string) ([]ProfileRequirement, error) {
	return m.requirements, m.reqErr
}

func (m *mockRepo) InsertRequirement(ctx context.Context, tx database.Tx, tenantID string, r *ProfileRequirement) (*ProfileRequirement, error) {
	return r, nil
}

func (m *mockRepo) InsertVersion(ctx context.Context, tx database.Tx, tenantID string, v *PieceProfileVersion) (*PieceProfileVersion, error) {
	if m.insertVerErr != nil {
		return nil, m.insertVerErr
	}
	if m.insertedVersion != nil {
		return m.insertedVersion, nil
	}
	return v, nil
}

func (m *mockRepo) GetVersionByKeyAndVersion(ctx context.Context, tx database.Tx, tenantID, profileKey, version string) (*PieceProfileVersion, error) {
	return nil, ErrPieceProfileVersionNotFound
}

func (m *mockRepo) GetMatterByKey(ctx context.Context, tx database.Tx, key string) (*Matter, error) {
	if m.matterErr != nil {
		return nil, m.matterErr
	}
	return m.matter, nil
}

func (m *mockRepo) ListMatters(ctx context.Context, tx database.Tx) ([]Matter, error) {
	if m.matter == nil {
		return nil, nil
	}
	return []Matter{*m.matter}, nil
}

func (m *mockRepo) GetBaseSkeletonByKey(ctx context.Context, tx database.Tx, key string) (*BaseSkeleton, error) {
	if m.baseSkeletonErr != nil {
		return nil, m.baseSkeletonErr
	}
	return m.baseSkeleton, nil
}

func (m *mockRepo) ListBaseSkeletons(ctx context.Context, tx database.Tx) ([]BaseSkeleton, error) {
	return nil, nil
}

func (m *mockRepo) GetFormatProfileByKey(ctx context.Context, tx database.Tx, key string) (*FormatProfile, error) {
	if m.formatProfileErr != nil {
		return nil, m.formatProfileErr
	}
	return m.formatProfile, nil
}

func (m *mockRepo) ListFormatProfiles(ctx context.Context, tx database.Tx) ([]FormatProfile, error) {
	return nil, nil
}

func TestUseCase_CreateProfile(t *testing.T) {
	t.Parallel()

	t.Run("happy path publishes piece_profile.created", func(t *testing.T) {
		t.Parallel()
		repo := &mockRepo{
			matter:       &Matter{Key: "civel", Nome: "Cível"},
			baseSkeleton: &BaseSkeleton{Key: "default"},
		}
		uow := &fakeUOW{}
		outbox := &recordingOutbox{}
		uc := NewUseCase(uow, repo, WithOutbox(outbox))

		p, err := uc.CreateProfile(context.Background(), CreateProfileCommand{
			TenantID: "tenant-1", Key: "contestacao", Nome: "Contestação",
			Polo: PoloPassivo, MatterKey: "civel", BaseSkeletonKey: "default",
		})
		if err != nil {
			t.Fatalf("CreateProfile() error = %v", err)
		}
		if p.Key != "contestacao" {
			t.Errorf("Key = %q, want contestacao", p.Key)
		}
		if p.VersionAtual != "v1" {
			t.Errorf("VersionAtual = %q, want v1 (initial label, not a counter)", p.VersionAtual)
		}
		if uow.scope != "tenant-1" {
			t.Errorf("uow scope = %q, want tenant-1", uow.scope)
		}
		if len(outbox.published) != 1 || outbox.published[0].Type() != TypePieceProfileCreated {
			t.Errorf("published = %+v, want one piece_profile.created event", outbox.published)
		}
	})

	t.Run("unknown matter propagates typed error", func(t *testing.T) {
		t.Parallel()
		repo := &mockRepo{matterErr: ErrMatterNotFound}
		uc := NewUseCase(&fakeUOW{}, repo)

		_, err := uc.CreateProfile(context.Background(), CreateProfileCommand{
			TenantID: "tenant-1", Key: "contestacao", MatterKey: "invalida",
		})
		if !errors.Is(err, ErrMatterNotFound) {
			t.Errorf("error = %v, want ErrMatterNotFound", err)
		}
	})

	t.Run("optional format_profile_key is not validated when empty", func(t *testing.T) {
		t.Parallel()
		repo := &mockRepo{
			matter:           &Matter{Key: "civel"},
			baseSkeleton:     &BaseSkeleton{Key: "default"},
			formatProfileErr: ErrFormatProfileNotFound, // must NOT be called
		}
		uc := NewUseCase(&fakeUOW{}, repo)

		_, err := uc.CreateProfile(context.Background(), CreateProfileCommand{
			TenantID: "tenant-1", Key: "contestacao", MatterKey: "civel", BaseSkeletonKey: "default",
			FormatProfileKey: "",
		})
		if err != nil {
			t.Fatalf("CreateProfile() error = %v, want nil (format_profile_key is optional)", err)
		}
	})
}

func TestUseCase_GetProfile(t *testing.T) {
	t.Parallel()

	repo := &mockRepo{
		getProfile:   &PieceProfile{Key: "contestacao"},
		sections:     []ProfileSection{{ID: "s1", Key: "merito"}},
		requirements: []ProfileRequirement{{ID: "r1", Campo: "valor_causa"}},
	}
	uc := NewUseCase(&fakeUOW{}, repo)

	p, err := uc.GetProfile(context.Background(), "tenant-1", "contestacao")
	if err != nil {
		t.Fatalf("GetProfile() error = %v", err)
	}
	if len(p.Sections) != 1 || len(p.Requirements) != 1 {
		t.Errorf("GetProfile() did not aggregate sections/requirements: %+v", p)
	}
}

func TestUseCase_CreateVersion(t *testing.T) {
	t.Parallel()

	t.Run("uses the caller-supplied version label, not an increment", func(t *testing.T) {
		t.Parallel()
		repo := &mockRepo{getProfile: &PieceProfile{Key: "contestacao", VersionAtual: "v1"}}
		outbox := &recordingOutbox{}
		uc := NewUseCase(&fakeUOW{}, repo, WithOutbox(outbox), WithClock(func() time.Time { return time.Unix(0, 0) }))

		v, err := uc.CreateVersion(context.Background(), CreateVersionCommand{
			TenantID: "tenant-1", ProfileKey: "contestacao", Version: "2025-09-01",
		})
		if err != nil {
			t.Fatalf("CreateVersion() error = %v", err)
		}
		if v.Version != "2025-09-01" {
			t.Errorf("Version = %q, want the explicit caller-supplied label", v.Version)
		}
		if len(outbox.published) != 1 || outbox.published[0].Type() != TypePieceProfileVersionCreated {
			t.Errorf("published = %+v, want one piece_profile.version_created event", outbox.published)
		}
	})

	t.Run("empty version is rejected before touching the repo", func(t *testing.T) {
		t.Parallel()
		uc := NewUseCase(&fakeUOW{}, &mockRepo{})

		_, err := uc.CreateVersion(context.Background(), CreateVersionCommand{
			TenantID: "tenant-1", ProfileKey: "contestacao", Version: "",
		})
		if err == nil {
			t.Fatal("CreateVersion() error = nil, want a validation error for empty version")
		}
	})
}

func TestUseCase_ReferenceCatalog(t *testing.T) {
	t.Parallel()

	repo := &mockRepo{
		matter:           &Matter{Key: "civel", Nome: "Cível"},
		baseSkeleton:     &BaseSkeleton{Key: "default"},
		formatProfile:    &FormatProfile{Key: "default", Fonte: "Times New Roman"},
		formatProfileErr: nil,
	}
	uc := NewUseCase(&fakeUOW{}, repo)

	t.Run("ListMatters", func(t *testing.T) {
		t.Parallel()
		matters, err := uc.ListMatters(context.Background())
		if err != nil {
			t.Fatalf("ListMatters() error = %v", err)
		}
		if len(matters) != 1 || matters[0].Key != "civel" {
			t.Errorf("ListMatters() = %+v, want [civel]", matters)
		}
	})

	t.Run("GetFormatProfile", func(t *testing.T) {
		t.Parallel()
		fp, err := uc.GetFormatProfile(context.Background(), "default")
		if err != nil {
			t.Fatalf("GetFormatProfile() error = %v", err)
		}
		if fp.Fonte != "Times New Roman" {
			t.Errorf("Fonte = %q, want Times New Roman", fp.Fonte)
		}
	})
}
