package court

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jusassessoria/platform/internal/court/courtdb"
	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/vault"
)

// courtConnectorID/Version identify this slice's sync_run rows — the same
// discriminator DJEN/DATAJUD already use on the shared table (ConnectorID column).
const (
	courtConnectorID      = "EPROC"
	courtConnectorVersion = "v1"
)

// FetchStateItem is one due record from ListDueFetchState — the use case's pull
// query for FetchAutosBatch.
type FetchStateItem struct {
	CourtRecordID string
	CNJNumber     string
	ObservedAt    time.Time
	LastFetchedAt *time.Time
	DocketCursor  *time.Time
}

// Repository is the port the use case sees — court_connection CRUD, the
// tenant_secret operations it needs (Insert/Get a sealed secret; tenant_secret has
// no notion of "what" it holds, so this slice's repository is a fine place for its
// queries, same as any other consumer would define its own — see migration 0069's
// comment), and FetchAutosBatch's own state (court_fetch_state, sync_run,
// AcquireTenantWriteLock — the last one a slice-owned repository operation, same
// pattern as internal/acquisition's own, not importable across slices). Every
// method is tenant-scoped (barrier 1, where applicable) and runs inside the
// caller's tx (UoW in domain.go), RLS as barrier 2.
type Repository interface {
	Insert(ctx context.Context, tx database.Tx, conn *CourtConnection) (id string, createdAt time.Time, err error)
	GetByID(ctx context.Context, tx database.Tx, tenantID, id string) (*CourtConnection, error)
	List(ctx context.Context, tx database.Tx, tenantID string) ([]CourtConnection, error)
	FindByCourtSystem(ctx context.Context, tx database.Tx, tenantID, court, system string) (*CourtConnection, error)
	UpdateStatus(ctx context.Context, tx database.Tx, tenantID, id string, status Status, lastAuthenticatedAt *time.Time, errMsg string) error
	UpdateMFASeedRef(ctx context.Context, tx database.Tx, tenantID, id, mfaSeedRef string) error
	UpdateSessionRef(ctx context.Context, tx database.Tx, tenantID, id, sessionRef string, lastAuthenticatedAt time.Time) error

	InsertSecret(ctx context.Context, tx database.Tx, tenantID string, sealed vault.Sealed) (id string, err error)
	GetSecret(ctx context.Context, tx database.Tx, tenantID, id string) (vault.Sealed, error)

	AcquireTenantWriteLock(ctx context.Context, tx database.Tx, tenantID string) error
	UpsertFetchStateObserved(ctx context.Context, tx database.Tx, tenantID, connectionID, recordID, cnjNumber string, observedAt time.Time) error
	ListDueFetchState(ctx context.Context, tx database.Tx, connectionID string, limit int32) ([]FetchStateItem, error)
	GetFetchState(ctx context.Context, tx database.Tx, connectionID, recordID string) (FetchStateItem, error)
	MarkFetchStateFetched(ctx context.Context, tx database.Tx, connectionID, recordID string, fetchedAt time.Time, docketCursor *time.Time) error

	OpenSyncRun(ctx context.Context, tx database.Tx, tenantID string, startedAt time.Time) (runID string, err error)
	CloseSyncRun(ctx context.Context, tx database.Tx, runID, status string, itemsNew, itemsDeduped int, finishedAt time.Time, errMsg string) error
}

// NewRepository returns the pgx implementation of Repository.
func NewRepository() Repository { return &pgRepository{} }

type pgRepository struct{}

func (r *pgRepository) Insert(ctx context.Context, tx database.Tx, conn *CourtConnection) (string, time.Time, error) {
	tid, err := uuid.Parse(conn.TenantID)
	if err != nil {
		return "", time.Time{}, apperr.NewInvalid("tenant id inválido")
	}
	uid, err := uuid.Parse(conn.AppUserID)
	if err != nil {
		return "", time.Time{}, apperr.NewInvalid("app user id inválido")
	}
	credRef, err := nullUUID(conn.CredentialRef)
	if err != nil {
		return "", time.Time{}, apperr.NewInvalid("credential ref inválido")
	}
	certRef, err := nullUUID(conn.CertificateRef)
	if err != nil {
		return "", time.Time{}, apperr.NewInvalid("certificate ref inválido")
	}

	row, err := courtdb.New(tx).InsertCourtConnection(ctx, courtdb.InsertCourtConnectionParams{
		TenantID:             tid,
		AppUserID:            uid,
		Court:                conn.Court,
		System:               conn.System,
		AuthenticationMethod: string(conn.AuthenticationMethod),
		CredentialRef:        credRef,
		CertificateRef:       certRef,
		Status:               string(conn.Status),
	})
	if err != nil {
		if isUniqueViolation(err) {
			return "", time.Time{}, ErrConnectionAlreadyExists
		}
		return "", time.Time{}, database.WrapInfra(err)
	}
	return row.ID.String(), row.CreatedAt.Time, nil
}

func (r *pgRepository) GetByID(ctx context.Context, tx database.Tx, tenantID, id string) (*CourtConnection, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, apperr.NewInvalid("tenant id inválido")
	}
	cid, err := uuid.Parse(id)
	if err != nil {
		return nil, apperr.NewInvalid("id da conexão inválido")
	}
	row, err := courtdb.New(tx).GetCourtConnectionByID(ctx, courtdb.GetCourtConnectionByIDParams{ID: cid, TenantID: tid})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrConnectionNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	conn := toCourtConnection(row)
	return &conn, nil
}

func (r *pgRepository) List(ctx context.Context, tx database.Tx, tenantID string) ([]CourtConnection, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, apperr.NewInvalid("tenant id inválido")
	}
	rows, err := courtdb.New(tx).ListCourtConnectionsByTenant(ctx, tid)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]CourtConnection, 0, len(rows))
	for _, row := range rows {
		out = append(out, toCourtConnection(row))
	}
	return out, nil
}

func (r *pgRepository) UpdateStatus(ctx context.Context, tx database.Tx, tenantID, id string, status Status, lastAuthenticatedAt *time.Time, errMsg string) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return apperr.NewInvalid("tenant id inválido")
	}
	cid, err := uuid.Parse(id)
	if err != nil {
		return apperr.NewInvalid("id da conexão inválido")
	}
	var lastAuth pgtype.Timestamptz
	if lastAuthenticatedAt != nil {
		lastAuth = pgtype.Timestamptz{Time: *lastAuthenticatedAt, Valid: true}
	}
	errJSON, err := encodeError(errMsg)
	if err != nil {
		return apperr.NewInfra("codificar erro da conexão", err)
	}
	_, err = courtdb.New(tx).UpdateCourtConnectionStatus(ctx, courtdb.UpdateCourtConnectionStatusParams{
		ID:                  cid,
		TenantID:            tid,
		Status:              string(status),
		LastAuthenticatedAt: lastAuth,
		Error:               errJSON,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConnectionNotFound
	}
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

func (r *pgRepository) UpdateMFASeedRef(ctx context.Context, tx database.Tx, tenantID, id, mfaSeedRef string) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return apperr.NewInvalid("tenant id inválido")
	}
	cid, err := uuid.Parse(id)
	if err != nil {
		return apperr.NewInvalid("id da conexão inválido")
	}
	seedRef, err := nullUUID(mfaSeedRef)
	if err != nil {
		return apperr.NewInvalid("mfa seed ref inválido")
	}
	_, err = courtdb.New(tx).UpdateCourtConnectionMFASeedRef(ctx, courtdb.UpdateCourtConnectionMFASeedRefParams{
		ID:         cid,
		TenantID:   tid,
		MfaSeedRef: seedRef,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConnectionNotFound
	}
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

func (r *pgRepository) UpdateSessionRef(ctx context.Context, tx database.Tx, tenantID, id, sessionRef string, lastAuthenticatedAt time.Time) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return apperr.NewInvalid("tenant id inválido")
	}
	cid, err := uuid.Parse(id)
	if err != nil {
		return apperr.NewInvalid("id da conexão inválido")
	}
	sessRef, err := nullUUID(sessionRef)
	if err != nil {
		return apperr.NewInvalid("session ref inválido")
	}
	_, err = courtdb.New(tx).UpdateCourtConnectionSessionRef(ctx, courtdb.UpdateCourtConnectionSessionRefParams{
		ID:                  cid,
		TenantID:            tid,
		SessionRef:          sessRef,
		LastAuthenticatedAt: pgtype.Timestamptz{Time: lastAuthenticatedAt, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConnectionNotFound
	}
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

func (r *pgRepository) FindByCourtSystem(ctx context.Context, tx database.Tx, tenantID, court, system string) (*CourtConnection, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return nil, apperr.NewInvalid("tenant id inválido")
	}
	row, err := courtdb.New(tx).FindCourtConnectionByCourtSystem(ctx, courtdb.FindCourtConnectionByCourtSystemParams{
		TenantID: tid,
		Court:    court,
		System:   system,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrConnectionNotFound
	}
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	conn := toCourtConnection(row)
	return &conn, nil
}

func (r *pgRepository) AcquireTenantWriteLock(ctx context.Context, tx database.Tx, tenantID string) error {
	if err := courtdb.New(tx).AcquireTenantWriteLock(ctx, tenantID); err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

func (r *pgRepository) UpsertFetchStateObserved(ctx context.Context, tx database.Tx, tenantID, connectionID, recordID, cnjNumber string, observedAt time.Time) error {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return apperr.NewInvalid("tenant id inválido")
	}
	cid, err := uuid.Parse(connectionID)
	if err != nil {
		return apperr.NewInvalid("id da conexão inválido")
	}
	rid, err := uuid.Parse(recordID)
	if err != nil {
		return apperr.NewInvalid("id do processo inválido")
	}
	err = courtdb.New(tx).UpsertCourtFetchStateObserved(ctx, courtdb.UpsertCourtFetchStateObservedParams{
		TenantID:          tid,
		CourtConnectionID: cid,
		CourtRecordID:     rid,
		CnjNumber:         cnjNumber,
		ObservedAt:        pgtype.Timestamptz{Time: observedAt, Valid: true},
	})
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

func (r *pgRepository) ListDueFetchState(ctx context.Context, tx database.Tx, connectionID string, limit int32) ([]FetchStateItem, error) {
	cid, err := uuid.Parse(connectionID)
	if err != nil {
		return nil, apperr.NewInvalid("id da conexão inválido")
	}
	rows, err := courtdb.New(tx).ListDueCourtFetchState(ctx, courtdb.ListDueCourtFetchStateParams{
		CourtConnectionID: cid,
		Limit:             limit,
	})
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	out := make([]FetchStateItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, FetchStateItem{
			CourtRecordID: row.CourtRecordID.String(),
			CNJNumber:     row.CnjNumber,
			ObservedAt:    row.ObservedAt.Time.UTC(),
			LastFetchedAt: timePtr(row.LastFetchedAt),
			DocketCursor:  timePtr(row.DocketCursor),
		})
	}
	return out, nil
}

func (r *pgRepository) GetFetchState(ctx context.Context, tx database.Tx, connectionID, recordID string) (FetchStateItem, error) {
	cid, err := uuid.Parse(connectionID)
	if err != nil {
		return FetchStateItem{}, apperr.NewInvalid("id da conexão inválido")
	}
	rid, err := uuid.Parse(recordID)
	if err != nil {
		return FetchStateItem{}, apperr.NewInvalid("id do processo inválido")
	}
	row, err := courtdb.New(tx).GetCourtFetchState(ctx, courtdb.GetCourtFetchStateParams{
		CourtConnectionID: cid,
		CourtRecordID:     rid,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return FetchStateItem{}, apperr.NewNotFound("estado de busca não encontrado")
	}
	if err != nil {
		return FetchStateItem{}, database.WrapInfra(err)
	}
	return FetchStateItem{
		CourtRecordID: row.CourtRecordID.String(),
		CNJNumber:     row.CnjNumber,
		ObservedAt:    row.ObservedAt.Time.UTC(),
		LastFetchedAt: timePtr(row.LastFetchedAt),
		DocketCursor:  timePtr(row.DocketCursor),
	}, nil
}

func (r *pgRepository) MarkFetchStateFetched(ctx context.Context, tx database.Tx, connectionID, recordID string, fetchedAt time.Time, docketCursor *time.Time) error {
	cid, err := uuid.Parse(connectionID)
	if err != nil {
		return apperr.NewInvalid("id da conexão inválido")
	}
	rid, err := uuid.Parse(recordID)
	if err != nil {
		return apperr.NewInvalid("id do processo inválido")
	}
	var cursor pgtype.Timestamptz
	if docketCursor != nil {
		cursor = pgtype.Timestamptz{Time: *docketCursor, Valid: true}
	}
	err = courtdb.New(tx).MarkCourtFetchStateFetched(ctx, courtdb.MarkCourtFetchStateFetchedParams{
		CourtConnectionID: cid,
		CourtRecordID:     rid,
		LastFetchedAt:     pgtype.Timestamptz{Time: fetchedAt, Valid: true},
		DocketCursor:      cursor,
	})
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

func (r *pgRepository) OpenSyncRun(ctx context.Context, tx database.Tx, tenantID string, startedAt time.Time) (string, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return "", apperr.NewInvalid("tenant id inválido")
	}
	integrationID, err := courtdb.New(tx).FindOrCreateEprocIntegration(ctx, tid)
	if err != nil {
		return "", database.WrapInfra(err)
	}
	id, err := courtdb.New(tx).InsertCourtSyncRun(ctx, courtdb.InsertCourtSyncRunParams{
		TenantID:         tid,
		IntegrationID:    integrationID,
		ConnectorID:      courtConnectorID,
		ConnectorVersion: courtConnectorVersion,
		StartedAt:        pgtype.Timestamptz{Time: startedAt, Valid: true},
		Status:           "RUNNING",
	})
	if err != nil {
		return "", database.WrapInfra(err)
	}
	return id.String(), nil
}

// CloseSyncRun mirrors acquisition's UpdateSyncRun RUNNING-only guard: pgx.ErrNoRows
// means another execution already closed this run (a redelivered task) — a no-op,
// not an error.
func (r *pgRepository) CloseSyncRun(ctx context.Context, tx database.Tx, runID, status string, itemsNew, itemsDeduped int, finishedAt time.Time, errMsg string) error {
	rid, err := uuid.Parse(runID)
	if err != nil {
		return apperr.NewInvalid("sync run id inválido")
	}
	errJSON, err := encodeError(errMsg)
	if err != nil {
		return apperr.NewInfra("codificar erro do sync run", err)
	}
	_, err = courtdb.New(tx).UpdateCourtSyncRun(ctx, courtdb.UpdateCourtSyncRunParams{
		ID:           rid,
		Status:       status,
		ItemsNew:     int32(itemsNew),
		ItemsDeduped: int32(itemsDeduped),
		FinishedAt:   pgtype.Timestamptz{Time: finishedAt, Valid: true},
		Error:        errJSON,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

func (r *pgRepository) InsertSecret(ctx context.Context, tx database.Tx, tenantID string, sealed vault.Sealed) (string, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return "", apperr.NewInvalid("tenant id inválido")
	}
	row, err := courtdb.New(tx).InsertTenantSecret(ctx, courtdb.InsertTenantSecretParams{
		TenantID:   tid,
		Ciphertext: sealed.Ciphertext,
		Nonce:      sealed.Nonce,
		WrappedDek: sealed.DEKCiphertext,
		DekNonce:   sealed.DEKNonce,
	})
	if err != nil {
		return "", database.WrapInfra(err)
	}
	return row.String(), nil
}

func (r *pgRepository) GetSecret(ctx context.Context, tx database.Tx, tenantID, id string) (vault.Sealed, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return vault.Sealed{}, apperr.NewInvalid("tenant id inválido")
	}
	sid, err := uuid.Parse(id)
	if err != nil {
		return vault.Sealed{}, apperr.NewInvalid("secret id inválido")
	}
	row, err := courtdb.New(tx).GetTenantSecretByID(ctx, courtdb.GetTenantSecretByIDParams{ID: sid, TenantID: tid})
	if errors.Is(err, pgx.ErrNoRows) {
		return vault.Sealed{}, apperr.NewNotFound("segredo não encontrado")
	}
	if err != nil {
		return vault.Sealed{}, database.WrapInfra(err)
	}
	return vault.Sealed{
		Ciphertext:    row.Ciphertext,
		Nonce:         row.Nonce,
		DEKCiphertext: row.WrappedDek,
		DEKNonce:      row.DekNonce,
	}, nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func toCourtConnection(row courtdb.CourtConnection) CourtConnection {
	return CourtConnection{
		ID:                   row.ID.String(),
		TenantID:             row.TenantID.String(),
		AppUserID:            row.AppUserID.String(),
		Court:                row.Court,
		System:               row.System,
		AuthenticationMethod: AuthenticationMethod(row.AuthenticationMethod),
		CredentialRef:        uuidOrEmpty(row.CredentialRef),
		CertificateRef:       uuidOrEmpty(row.CertificateRef),
		MFASeedRef:           uuidOrEmpty(row.MfaSeedRef),
		SessionRef:           uuidOrEmpty(row.SessionRef),
		Status:               Status(row.Status),
		LastAuthenticatedAt:  timePtr(row.LastAuthenticatedAt),
		Error:                decodeError(row.Error),
		CreatedAt:            row.CreatedAt.Time.UTC(),
	}
}

func nullUUID(s string) (pgtype.UUID, error) {
	if s == "" {
		return pgtype.UUID{}, nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return pgtype.UUID{}, err
	}
	return pgtype.UUID{Bytes: id, Valid: true}, nil
}

func uuidOrEmpty(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}

func timePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	u := t.Time.UTC()
	return &u
}

// encodeError/decodeError round-trip the human-readable last-error message through
// the jsonb `error` column as a bare JSON string — no structure worth a richer shape
// yet (no stack, no code), and nil/"" persists as SQL NULL rather than `"null"`.
func encodeError(msg string) ([]byte, error) {
	if msg == "" {
		return nil, nil
	}
	return json.Marshal(msg)
}

func decodeError(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var msg string
	if err := json.Unmarshal(raw, &msg); err != nil {
		return string(raw)
	}
	return msg
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
