package pieceprofile

import "testing"

func TestCreateProfileRequest_Validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		req     CreateProfileRequest
		wantErr bool
	}{
		{
			name: "valid",
			req: CreateProfileRequest{
				Key: "contestacao", Nome: "Contestação", Polo: PoloPassivo,
				MatterKey: "civel", BaseSkeletonKey: "default",
			},
			wantErr: false,
		},
		{
			name:    "missing key",
			req:     CreateProfileRequest{Nome: "Contestação", Polo: PoloPassivo, MatterKey: "civel", BaseSkeletonKey: "default"},
			wantErr: true,
		},
		{
			name:    "missing nome",
			req:     CreateProfileRequest{Key: "contestacao", Polo: PoloPassivo, MatterKey: "civel", BaseSkeletonKey: "default"},
			wantErr: true,
		},
		{
			name:    "invalid polo",
			req:     CreateProfileRequest{Key: "contestacao", Nome: "Contestação", Polo: "invalido", MatterKey: "civel", BaseSkeletonKey: "default"},
			wantErr: true,
		},
		{
			name:    "missing matter_key",
			req:     CreateProfileRequest{Key: "contestacao", Nome: "Contestação", Polo: PoloPassivo, BaseSkeletonKey: "default"},
			wantErr: true,
		},
		{
			name: "format_profile_key optional (nullable column)",
			req: CreateProfileRequest{
				Key: "contestacao", Nome: "Contestação", Polo: PoloPassivo,
				MatterKey: "civel", BaseSkeletonKey: "default", FormatProfileKey: "",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.req.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCreateSectionRequest_Validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		req     CreateSectionRequest
		wantErr bool
	}{
		{
			name: "valid",
			req: CreateSectionRequest{
				Key: "merito", Titulo: "Do Mérito", Ordem: 1,
				Obrigatoria: ObrigatoriaSim, Origem: OrigemArgumentativa,
			},
			wantErr: false,
		},
		{
			name:    "invalid obrigatoria",
			req:     CreateSectionRequest{Key: "merito", Titulo: "Do Mérito", Ordem: 1, Obrigatoria: "talvez", Origem: OrigemArgumentativa},
			wantErr: true,
		},
		{
			name:    "invalid origem",
			req:     CreateSectionRequest{Key: "merito", Titulo: "Do Mérito", Ordem: 1, Obrigatoria: ObrigatoriaSim, Origem: "inventado"},
			wantErr: true,
		},
		{
			name:    "missing ordem",
			req:     CreateSectionRequest{Key: "merito", Titulo: "Do Mérito", Obrigatoria: ObrigatoriaSim, Origem: OrigemArgumentativa},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.req.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCreateVersionRequest_Validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		req     CreateVersionRequest
		wantErr bool
	}{
		{name: "valid explicit version", req: CreateVersionRequest{Version: "v1.1"}, wantErr: false},
		{name: "valid date-like version", req: CreateVersionRequest{Version: "2025-09-01"}, wantErr: false},
		{name: "missing version", req: CreateVersionRequest{Version: ""}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.req.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
