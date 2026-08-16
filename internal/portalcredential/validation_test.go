package portalcredential_test

import (
	"testing"

	"github.com/jusassessoria/platform/internal/portalcredential"
)

func TestConfigurePortalCredentialRequest_Validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		req     portalcredential.ConfigurePortalCredentialRequest
		wantErr bool
	}{
		{
			name:    "valid",
			req:     portalcredential.ConfigurePortalCredentialRequest{Login: "advogado@escritorio.com.br", Password: "senha-forte"},
			wantErr: false,
		},
		{
			name:    "missing login",
			req:     portalcredential.ConfigurePortalCredentialRequest{Password: "senha-forte"},
			wantErr: true,
		},
		{
			name:    "missing password",
			req:     portalcredential.ConfigurePortalCredentialRequest{Login: "advogado@escritorio.com.br"},
			wantErr: true,
		},
		{
			name:    "both missing",
			req:     portalcredential.ConfigurePortalCredentialRequest{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.req.Validate()
			if tt.wantErr && err == nil {
				t.Fatal("Validate() error = nil, want error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("Validate() error = %v, want nil", err)
			}
		})
	}
}
