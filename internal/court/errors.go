package court

import "github.com/jusassessoria/platform/lib/apperr"

// ErrConnectionNotFound: no court_connection matches (tenant, id).
var ErrConnectionNotFound = apperr.NewNotFound("conexão com o tribunal não encontrada")

// ErrConnectionAlreadyExists: (tenant, app_user, court, system) already has a row —
// the UNIQUE constraint court_connection enforces. The FE should update, not recreate.
var ErrConnectionAlreadyExists = apperr.NewConflict("já existe uma conexão para este advogado neste tribunal")

// ErrProviderNotRegistered: no CourtProvider wired for this (court, system) pair —
// a config/deploy problem (the slice didn't register an adapter), not a user error.
var ErrProviderNotRegistered = apperr.NewInfra("nenhum provedor registrado para este tribunal/sistema", nil)

// ErrMFAEnrollmentFailed: EnrollMFA ran but did not produce a usable seed (portal
// page shape unexpected, or the account already has MFA configured elsewhere — see
// EprocProvider.EnrollMFA's doc). The connection stays in
// MFA_ENROLLMENT_REQUIRED so a retry (after investigating) is a no-op resume, not a
// fresh attempt.
var ErrMFAEnrollmentFailed = apperr.NewUnavailable("não foi possível configurar o segundo fator automaticamente", nil)
