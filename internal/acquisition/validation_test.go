package acquisition

import "testing"

// req is a small builder for a valid ActivateIntegrationRequest that each case
// then perturbs, so every test starts from a known-good baseline.
func req(sources []string, oab []string) ActivateIntegrationRequest {
	return ActivateIntegrationRequest{
		Sources: sources,
		Scope:   Scope{OAB: oab},
	}
}

// TestActivateIntegrationRequest_Validate covers ACs 2, 3 and 4: the boundary
// rejects an empty/malformed scope and any source outside {DJEN} — DATAJUD is
// enrichment-only, not activatable — and accepts a well-formed request.
func TestActivateIntegrationRequest_Validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		request ActivateIntegrationRequest
		wantErr bool
	}{
		{
			name:    "valid single source",
			request: req([]string{SourceDJEN}, []string{"SP123456"}),
			wantErr: false,
		},
		{
			name:    "DATAJUD is not activatable (enrichment-only)",
			request: req([]string{SourceDJEN, SourceDATAJUD}, []string{"SP1", "RJ654321"}),
			wantErr: true,
		},
		// AC4: sources empty / unsupported.
		{
			name:    "sources empty is invalid",
			request: req([]string{}, []string{"SP123456"}),
			wantErr: true,
		},
		{
			name:    "source UPLOAD is invalid",
			request: req([]string{SourceUpload}, []string{"SP123456"}),
			wantErr: true,
		},
		{
			name:    "source MNI is invalid",
			request: req([]string{"MNI"}, []string{"SP123456"}),
			wantErr: true,
		},
		{
			name:    "one bad source among good ones is invalid",
			request: req([]string{SourceDJEN, "MNI"}, []string{"SP123456"}),
			wantErr: true,
		},
		// AC2: oab empty.
		{
			name:    "scope oab empty is invalid",
			request: req([]string{SourceDJEN}, []string{}),
			wantErr: true,
		},
		// AC3: oab regex.
		{
			name:    "oab lowercase uf is invalid",
			request: req([]string{SourceDJEN}, []string{"sp123456"}),
			wantErr: true,
		},
		{
			name:    "oab missing digits is invalid",
			request: req([]string{SourceDJEN}, []string{"SP"}),
			wantErr: true,
		},
		{
			name:    "oab too many digits is invalid",
			request: req([]string{SourceDJEN}, []string{"SP1234567"}),
			wantErr: true,
		},
		{
			name:    "one bad oab among good ones is invalid",
			request: req([]string{SourceDJEN}, []string{"SP123456", "bad"}),
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
