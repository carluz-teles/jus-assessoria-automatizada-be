package acquisition

import "testing"

// TestScope_Validate covers ACs 2 and 3: the boundary rejects an empty/malformed
// scope and accepts a well-formed one. There is no source selector — DJEN is the
// only activatable source (see domain.go).
func TestScope_Validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		oab     []string
		wantErr bool
	}{
		{
			name:    "valid scope",
			oab:     []string{"SP123456"},
			wantErr: false,
		},
		{
			name:    "valid multiple oabs",
			oab:     []string{"SP1", "RJ654321"},
			wantErr: false,
		},
		// AC2: oab empty.
		{
			name:    "scope oab empty is invalid",
			oab:     []string{},
			wantErr: true,
		},
		// AC3: oab regex.
		{
			name:    "oab lowercase uf is invalid",
			oab:     []string{"sp123456"},
			wantErr: true,
		},
		{
			name:    "oab missing digits is invalid",
			oab:     []string{"SP"},
			wantErr: true,
		},
		{
			name:    "oab too many digits is invalid",
			oab:     []string{"SP1234567"},
			wantErr: true,
		},
		{
			name:    "one bad oab among good ones is invalid",
			oab:     []string{"SP123456", "bad"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := Scope{OAB: tt.oab}.Validate()
			if tt.wantErr && err == nil {
				t.Fatalf("Validate() = nil, want an error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}
