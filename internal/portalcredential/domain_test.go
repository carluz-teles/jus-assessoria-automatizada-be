package portalcredential_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jusassessoria/platform/internal/portalcredential"
	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/vault"
)

// --- fakes -------------------------------------------------------------------

// fakeUow runs fn directly against a nil Tx — the fakeRepo below never touches
// it, so no real transaction/connection is needed to exercise the use case.
type fakeUow struct {
	doErr error
}

func (f fakeUow) Do(_ context.Context, _ string, fn func(database.Tx) error) error {
	if f.doErr != nil {
		return f.doErr
	}
	return fn(nil)
}

func (f fakeUow) DoSystem(_ context.Context, fn func(database.Tx) error) error {
	return fn(nil)
}

// fakeChecker returns a canned LoginResult/error and records what it was called
// with — the login/password test double for domain.go's Configure.
type fakeChecker struct {
	result LoginResultAlias
	err    error

	gotLogin    string
	gotPassword string
}

// LoginResultAlias avoids importing the unexported type name churn: it is
// literally portalcredential.LoginResult, aliased for readability at call sites
// below (Go type aliases carry no runtime cost).
type LoginResultAlias = portalcredential.LoginResult

func (f *fakeChecker) Check(_ context.Context, login, password string) (LoginResultAlias, error) {
	f.gotLogin, f.gotPassword = login, password
	return f.result, f.err
}

// fakeRepo is an in-memory stand-in for Repository — a map keyed by
// tenant+user+portal, and a separate map for sealed secrets keyed by id. It
// mirrors exactly the shape domain.go's Configure/Get/Delete need.
type fakeRepo struct {
	credentials map[string]*portalcredential.PortalCredential
	secrets     map[string]vault.Sealed
	nextID      int
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		credentials: map[string]*portalcredential.PortalCredential{},
		secrets:     map[string]vault.Sealed{},
	}
}

func credKey(tenantID, appUserID, portal string) string { return tenantID + "|" + appUserID + "|" + portal }

func (r *fakeRepo) InsertSecret(_ context.Context, _ database.Tx, _ string, sealed vault.Sealed) (string, error) {
	r.nextID++
	id := "secret-" + string(rune('a'+r.nextID))
	r.secrets[id] = sealed
	return id, nil
}

func (r *fakeRepo) GetSecret(_ context.Context, _ database.Tx, _, secretID string) (vault.Sealed, error) {
	s, ok := r.secrets[secretID]
	if !ok {
		return vault.Sealed{}, apperr.NewNotFound("segredo não encontrado")
	}
	return s, nil
}

func (r *fakeRepo) DeleteSecret(_ context.Context, _ database.Tx, _, secretID string) error {
	delete(r.secrets, secretID)
	return nil
}

func (r *fakeRepo) UpsertPortalCredential(_ context.Context, _ database.Tx, params portalcredential.UpsertPortalCredentialParams) (*portalcredential.PortalCredential, error) {
	cred := &portalcredential.PortalCredential{
		ID:             "cred-1",
		TenantID:       params.TenantID,
		AppUserID:      params.AppUserID,
		Portal:         params.Portal,
		Login:          params.Login,
		CredentialRef:  params.CredentialRef,
		Status:         params.Status,
		LastError:      params.LastError,
		LastVerifiedAt: params.LastVerifiedAt,
		ConfiguredBy:   params.ConfiguredBy,
	}
	r.credentials[credKey(params.TenantID, params.AppUserID, params.Portal)] = cred
	return cred, nil
}

func (r *fakeRepo) GetPortalCredential(_ context.Context, _ database.Tx, tenantID, appUserID, portal string) (*portalcredential.PortalCredential, error) {
	cred, ok := r.credentials[credKey(tenantID, appUserID, portal)]
	if !ok {
		return nil, portalcredential.ErrPortalCredentialNotFound
	}
	return cred, nil
}

func (r *fakeRepo) DeletePortalCredential(_ context.Context, _ database.Tx, tenantID, appUserID, portal string) error {
	delete(r.credentials, credKey(tenantID, appUserID, portal))
	return nil
}

// testVault builds a real lib/vault.Vault (it is cheap, pure crypto — no reason
// to fake it) so Seal/Open round trips are genuinely exercised.
func testVault(t *testing.T) *vault.Vault {
	t.Helper()
	kek, err := vault.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK() error = %v", err)
	}
	v, err := vault.New(kek)
	if err != nil {
		t.Fatalf("vault.New() error = %v", err)
	}
	return v
}

const (
	testTenantID = "11111111-1111-1111-1111-111111111111"
	testUserID   = "22222222-2222-2222-2222-222222222222"
)

// --- Configure ----------------------------------------------------------------

func TestUseCase_Configure_Success_PersistsActiveAndSealsPassword(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo()
	checker := &fakeChecker{result: LoginResultAlias{Outcome: portalcredential.LoginOutcomeSuccess}}
	uc := portalcredential.NewUseCase(repo, checker, testVault(t), fakeUow{})

	cred, err := uc.Configure(context.Background(), testTenantID, testUserID, "advogado", "senha-correta")
	if err != nil {
		t.Fatalf("Configure() error = %v", err)
	}
	if cred.Status != portalcredential.StatusActive {
		t.Errorf("Status = %q, want %q", cred.Status, portalcredential.StatusActive)
	}
	if cred.LastVerifiedAt.IsZero() {
		t.Error("LastVerifiedAt is zero, want it set on a successful test")
	}
	if cred.LastError != "" {
		t.Errorf("LastError = %q, want empty on success", cred.LastError)
	}
	if checker.gotLogin != "advogado" || checker.gotPassword != "senha-correta" {
		t.Errorf("checker got (%q, %q), want (advogado, senha-correta)", checker.gotLogin, checker.gotPassword)
	}

	// The password must never be recoverable from the persisted entity — only
	// via the sealed secret the credential_ref points to.
	sealed, ok := repo.secrets[cred.CredentialRef]
	if !ok {
		t.Fatal("no sealed secret was stored for the returned credential_ref")
	}
	if string(sealed.Ciphertext) == "senha-correta" {
		t.Error("the stored secret's ciphertext equals the plaintext password")
	}
}

func TestUseCase_Configure_Rejected_NeverPersists(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo()
	checker := &fakeChecker{result: LoginResultAlias{Outcome: portalcredential.LoginOutcomeRejected, Detail: "usuário ou senha inválidos"}}
	uc := portalcredential.NewUseCase(repo, checker, testVault(t), fakeUow{})

	_, err := uc.Configure(context.Background(), testTenantID, testUserID, "advogado", "senha-errada")
	if !errors.Is(err, portalcredential.ErrPortalRejectedCredential) {
		t.Fatalf("Configure() error = %v, want ErrPortalRejectedCredential", err)
	}
	if _, ok := repo.credentials[credKey(testTenantID, testUserID, portalcredential.PortalTJSPEproc)]; ok {
		t.Error("a rejected credential was persisted")
	}
	if len(repo.secrets) != 0 {
		t.Error("a rejected credential's password was sealed and stored")
	}
}

func TestUseCase_Configure_Inconclusive_StillPersistsAsAuthFailed(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo()
	checker := &fakeChecker{result: LoginResultAlias{
		Outcome: portalcredential.LoginOutcomeInconclusive,
		Detail:  "portal indisponível ou tempo esgotado ao abrir a página de login",
	}}
	uc := portalcredential.NewUseCase(repo, checker, testVault(t), fakeUow{})

	cred, err := uc.Configure(context.Background(), testTenantID, testUserID, "advogado", "senha-qualquer")
	if err != nil {
		t.Fatalf("Configure() error = %v, want nil (inconclusive still saves, per the ERD's honest-degradation rule)", err)
	}
	if cred.Status != portalcredential.StatusAuthFailed {
		t.Errorf("Status = %q, want %q", cred.Status, portalcredential.StatusAuthFailed)
	}
	if cred.LastError == "" {
		t.Error("LastError is empty, want the inconclusive detail carried through")
	}
	if !cred.LastVerifiedAt.IsZero() {
		t.Error("LastVerifiedAt is set, want zero (never successfully verified)")
	}
}

func TestUseCase_Configure_Reconfigure_DeletesOldSecret(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo()
	checker := &fakeChecker{result: LoginResultAlias{Outcome: portalcredential.LoginOutcomeSuccess}}
	uc := portalcredential.NewUseCase(repo, checker, testVault(t), fakeUow{})

	first, err := uc.Configure(context.Background(), testTenantID, testUserID, "advogado", "senha-1")
	if err != nil {
		t.Fatalf("first Configure() error = %v", err)
	}
	firstSecretRef := first.CredentialRef

	second, err := uc.Configure(context.Background(), testTenantID, testUserID, "advogado", "senha-2")
	if err != nil {
		t.Fatalf("second Configure() error = %v", err)
	}

	if second.CredentialRef == firstSecretRef {
		t.Error("reconfigure reused the same credential_ref — expected a fresh sealed secret")
	}
	if _, stillThere := repo.secrets[firstSecretRef]; stillThere {
		t.Error("the OLD secret was not deleted after a reconfigure")
	}
	if len(repo.credentials) != 1 {
		t.Errorf("credentials map has %d entries, want exactly 1 (upsert, not a second row)", len(repo.credentials))
	}
}

func TestUseCase_Configure_ContextError_PropagatesWithoutPersisting(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo()
	checker := &fakeChecker{err: context.Canceled}
	uc := portalcredential.NewUseCase(repo, checker, testVault(t), fakeUow{})

	_, err := uc.Configure(context.Background(), testTenantID, testUserID, "advogado", "senha")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Configure() error = %v, want context.Canceled", err)
	}
	if len(repo.credentials) != 0 {
		t.Error("a credential was persisted despite the checker's context error")
	}
}

// --- Get / Delete --------------------------------------------------------------

func TestUseCase_Get_NotConfigured_ReturnsTypedNotFound(t *testing.T) {
	t.Parallel()

	uc := portalcredential.NewUseCase(newFakeRepo(), &fakeChecker{}, testVault(t), fakeUow{})

	_, err := uc.Get(context.Background(), testTenantID, testUserID)
	if !errors.Is(err, portalcredential.ErrPortalCredentialNotFound) {
		t.Fatalf("Get() error = %v, want ErrPortalCredentialNotFound", err)
	}
}

func TestUseCase_Get_ReturnsConfiguredCredential(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo()
	checker := &fakeChecker{result: LoginResultAlias{Outcome: portalcredential.LoginOutcomeSuccess}}
	uc := portalcredential.NewUseCase(repo, checker, testVault(t), fakeUow{})

	if _, err := uc.Configure(context.Background(), testTenantID, testUserID, "advogado", "senha"); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	cred, err := uc.Get(context.Background(), testTenantID, testUserID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if cred.Login != "advogado" {
		t.Errorf("Login = %q, want %q", cred.Login, "advogado")
	}
}

func TestUseCase_Delete_RemovesCredentialAndSecret(t *testing.T) {
	t.Parallel()

	repo := newFakeRepo()
	checker := &fakeChecker{result: LoginResultAlias{Outcome: portalcredential.LoginOutcomeSuccess}}
	uc := portalcredential.NewUseCase(repo, checker, testVault(t), fakeUow{})

	cred, err := uc.Configure(context.Background(), testTenantID, testUserID, "advogado", "senha")
	if err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	if err := uc.Delete(context.Background(), testTenantID, testUserID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, ok := repo.credentials[credKey(testTenantID, testUserID, portalcredential.PortalTJSPEproc)]; ok {
		t.Error("credential still present after Delete")
	}
	if _, ok := repo.secrets[cred.CredentialRef]; ok {
		t.Error("sealed secret still present after Delete")
	}
}

func TestUseCase_Delete_NotConfigured_ReturnsTypedNotFound(t *testing.T) {
	t.Parallel()

	uc := portalcredential.NewUseCase(newFakeRepo(), &fakeChecker{}, testVault(t), fakeUow{})

	err := uc.Delete(context.Background(), testTenantID, testUserID)
	if !errors.Is(err, portalcredential.ErrPortalCredentialNotFound) {
		t.Fatalf("Delete() error = %v, want ErrPortalCredentialNotFound", err)
	}
}

