package acquisition

import (
	"strings"
	"testing"
)

// req is a small builder for a valid ActivateIntegrationRequest that each case
// then perturbs, so every test starts from a known-good baseline.
func req(oab []string) ActivateIntegrationRequest {
	return ActivateIntegrationRequest{
		Scope: Scope{OAB: oab},
	}
}

// TestActivateIntegrationRequest_Validate covers ACs 2 and 3: the boundary
// rejects an empty/malformed scope and accepts a well-formed request. There is
// no source selector — DJEN is the only activatable source (see domain.go).
func TestActivateIntegrationRequest_Validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		request ActivateIntegrationRequest
		wantErr bool
	}{
		{
			name:    "valid scope",
			request: req([]string{"SP123456"}),
			wantErr: false,
		},
		{
			name:    "valid multiple oabs",
			request: req([]string{"SP1", "RJ654321"}),
			wantErr: false,
		},
		// AC2: oab empty.
		{
			name:    "scope oab empty is invalid",
			request: req([]string{}),
			wantErr: true,
		},
		// AC3: oab regex.
		{
			name:    "oab lowercase uf is invalid",
			request: req([]string{"sp123456"}),
			wantErr: true,
		},
		{
			name:    "oab missing digits is invalid",
			request: req([]string{"SP"}),
			wantErr: true,
		},
		{
			name:    "oab too many digits is invalid",
			request: req([]string{"SP1234567"}),
			wantErr: true,
		},
		{
			name:    "one bad oab among good ones is invalid",
			request: req([]string{"SP123456", "bad"}),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.request.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("Validate() = nil, want an error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

// TestUpdateProcessoManualRequest_Validate covers the Label boundary rule added for
// Achado 1 (PATCH /v1/processos/:id): absent (nil) is a no-op, a well-formed value up to
// 255 runes is accepted, an empty string is accepted (it clears the manual título), and
// anything past 255 runes is a 400 at the edge. The pre-existing Phase/ClaimValue rules
// are left untouched (regression guard: they must keep working alongside Label).
func TestUpdateProcessoManualRequest_Validate(t *testing.T) {
	t.Parallel()

	label := func(s string) *string { return &s }
	phase := FaseInstrucao
	claim := 1000.0

	tests := []struct {
		name    string
		request UpdateProcessoManualRequest
		wantErr bool
	}{
		{
			name:    "all nil is valid (no-op PATCH)",
			request: UpdateProcessoManualRequest{},
			wantErr: false,
		},
		{
			name:    "label present, well-formed",
			request: UpdateProcessoManualRequest{Label: label("Ação de Cobrança — Cliente ACME")},
			wantErr: false,
		},
		{
			name:    "label empty string clears the manual título — valid",
			request: UpdateProcessoManualRequest{Label: label("")},
			wantErr: false,
		},
		{
			name:    "label at the 255-rune boundary is valid",
			request: UpdateProcessoManualRequest{Label: label(strings.Repeat("a", 255))},
			wantErr: false,
		},
		{
			name:    "label past 255 runes is invalid",
			request: UpdateProcessoManualRequest{Label: label(strings.Repeat("a", 256))},
			wantErr: true,
		},
		{
			name:    "label alongside valid phase/claim_value is valid",
			request: UpdateProcessoManualRequest{Label: label("Título"), Phase: &phase, ClaimValue: &claim},
			wantErr: false,
		},
		{
			name:    "an invalid phase still fails regardless of label",
			request: UpdateProcessoManualRequest{Label: label("Título"), Phase: strPtrTitle("NOT_A_FASE")},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.request.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("Validate() = nil, want an error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}
