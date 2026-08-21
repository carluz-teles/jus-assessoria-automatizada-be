package acquisition

import "testing"

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
