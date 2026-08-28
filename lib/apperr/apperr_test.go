package apperr_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jusassessoria/platform/lib/apperr"
)

func TestConstructors_SetKindAndMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		build    func() *apperr.AppError
		wantKind apperr.Kind
		wantMsg  string
	}{
		{
			name:     "invalid",
			build:    func() *apperr.AppError { return apperr.NewInvalid("tipo de peça inválido") },
			wantKind: apperr.KindInvalid,
			wantMsg:  "tipo de peça inválido",
		},
		{
			name:     "not found",
			build:    func() *apperr.AppError { return apperr.NewNotFound("minuta não encontrada") },
			wantKind: apperr.KindNotFound,
			wantMsg:  "minuta não encontrada",
		},
		{
			name:     "unauthorized",
			build:    func() *apperr.AppError { return apperr.NewUnauthorized("token inválido") },
			wantKind: apperr.KindUnauthorized,
			wantMsg:  "token inválido",
		},
		{
			name:     "forbidden",
			build:    func() *apperr.AppError { return apperr.NewForbidden("acesso negado") },
			wantKind: apperr.KindForbidden,
			wantMsg:  "acesso negado",
		},
		{
			name:     "conflict",
			build:    func() *apperr.AppError { return apperr.NewConflict("minuta já assinada") },
			wantKind: apperr.KindConflict,
			wantMsg:  "minuta já assinada",
		},
		{
			name:     "infra",
			build:    func() *apperr.AppError { return apperr.NewInfra("erro interno", errors.New("boom")) },
			wantKind: apperr.KindInfra,
			wantMsg:  "erro interno",
		},
		{
			name:     "unavailable",
			build:    func() *apperr.AppError { return apperr.NewUnavailable("provedor indisponível", errors.New("timeout")) },
			wantKind: apperr.KindUnavailable,
			wantMsg:  "provedor indisponível",
		},
		{
			name:     "rate limited",
			build:    func() *apperr.AppError { return apperr.NewRateLimited("too many requests") },
			wantKind: apperr.KindRateLimited,
			wantMsg:  "too many requests",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ae := tt.build()
			if ae.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q", ae.Kind, tt.wantKind)
			}
			if ae.Message != tt.wantMsg {
				t.Errorf("Message = %q, want %q", ae.Message, tt.wantMsg)
			}
		})
	}
}

func TestKind_Values(t *testing.T) {
	t.Parallel()

	// The edge (lib/httpx) keys statusByKind on these exact strings, and the
	// front end matches on them — pin the wire values against §4.3.
	tests := []struct {
		got  apperr.Kind
		want string
	}{
		{apperr.KindInvalid, "DOMAIN_ERROR_INVALID"},
		{apperr.KindUnauthorized, "AUTHENTICATION_ERROR"},
		{apperr.KindForbidden, "AUTHORIZATION_ERROR"},
		{apperr.KindNotFound, "ENTITY_NOT_FOUND"},
		{apperr.KindConflict, "CONFLICT"},
		{apperr.KindInfra, "INFRA_ERROR"},
		{apperr.KindUnavailable, "SERVICE_UNAVAILABLE"},
		{apperr.KindRateLimited, "RATE_LIMITED"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			t.Parallel()

			if string(tt.got) != tt.want {
				t.Errorf("Kind = %q, want %q", string(tt.got), tt.want)
			}
		})
	}
}

func TestError_IncludesKindAndMessage(t *testing.T) {
	t.Parallel()

	ae := apperr.NewInvalid("corpo inválido")
	msg := ae.Error()

	if msg == "" {
		t.Fatal("Error() returned empty string")
	}
	if !strings.Contains(msg, "corpo inválido") {
		t.Errorf("Error() = %q, want it to contain the Message", msg)
	}
	if !strings.Contains(msg, string(apperr.KindInvalid)) {
		t.Errorf("Error() = %q, want it to contain the Kind", msg)
	}
}

func TestError_IncludesCauseWhenPresent(t *testing.T) {
	t.Parallel()

	cause := errors.New("connection refused")
	ae := apperr.NewInfra("erro interno", cause)

	if !strings.Contains(ae.Error(), "connection refused") {
		t.Errorf("Error() = %q, want it to surface the cause for logs", ae.Error())
	}
}

func TestErrorsAs_ExtractsFromWrappedChain(t *testing.T) {
	t.Parallel()

	// A caller wraps an AppError with more context; errors.As must still reach it.
	base := apperr.NewNotFound("processo não encontrado")
	wrapped := fmt.Errorf("carregando pasta: %w", base)

	var ae *apperr.AppError
	if !errors.As(wrapped, &ae) {
		t.Fatal("errors.As did not extract *AppError from wrapped chain")
	}
	if ae.Kind != apperr.KindNotFound {
		t.Errorf("Kind = %q, want %q", ae.Kind, apperr.KindNotFound)
	}
}

func TestErrorsIs_ReachesInfraCause(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("pool exhausted")
	ae := apperr.NewInfra("erro interno", sentinel)

	if !errors.Is(ae, sentinel) {
		t.Error("errors.Is did not reach the wrapped cause via Unwrap")
	}
	if got := ae.Unwrap(); got != sentinel {
		t.Errorf("Unwrap() = %v, want the wrapped cause", got)
	}
}

func TestUnwrap_NilWhenNoCause(t *testing.T) {
	t.Parallel()

	if got := apperr.NewInvalid("x").Unwrap(); got != nil {
		t.Errorf("Unwrap() = %v, want nil for a cause-less error", got)
	}
}

func TestWithDetails_RoundTripsAndChains(t *testing.T) {
	t.Parallel()

	details := map[string]string{"tipo_peca": "valor deve ser um dos: CONTESTACAO, ..."}
	ae := apperr.NewInvalid("tipo de peça inválido").WithDetails(details)

	got, ok := ae.Details.(map[string]string)
	if !ok {
		t.Fatalf("Details = %T, want map[string]string", ae.Details)
	}
	if got["tipo_peca"] != details["tipo_peca"] {
		t.Errorf("Details round-trip = %v, want %v", got, details)
	}
	// WithDetails must return the same error so it chains off a constructor.
	if ae.Kind != apperr.KindInvalid || ae.Message != "tipo de peça inválido" {
		t.Errorf("WithDetails altered Kind/Message: %q / %q", ae.Kind, ae.Message)
	}
}

func TestFrom_ExtractsOrReportsAbsence(t *testing.T) {
	t.Parallel()

	t.Run("present in chain", func(t *testing.T) {
		t.Parallel()

		wrapped := fmt.Errorf("ctx: %w", apperr.NewConflict("já existe"))
		ae, ok := apperr.From(wrapped)
		if !ok {
			t.Fatal("From reported false for a chain containing *AppError")
		}
		if ae.Kind != apperr.KindConflict {
			t.Errorf("Kind = %q, want %q", ae.Kind, apperr.KindConflict)
		}
	})

	t.Run("absent", func(t *testing.T) {
		t.Parallel()

		ae, ok := apperr.From(errors.New("plain error"))
		if ok {
			t.Error("From reported true for a non-AppError")
		}
		if ae != nil {
			t.Errorf("From returned %v, want nil when absent", ae)
		}
	})
}
