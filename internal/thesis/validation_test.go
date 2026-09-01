package thesis

import "testing"

func TestCreateThesisRequest_Validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		req     CreateThesisRequest
		wantErr bool
	}{
		{
			name: "valid",
			req: CreateThesisRequest{
				DraftID: "draft-1", Enunciado: "réu não impugnou o fato X", Forca: ForcaFavoravel,
			},
			wantErr: false,
		},
		{
			name: "valid with anchors",
			req: CreateThesisRequest{
				DraftID: "draft-1", Enunciado: "e", Forca: ForcaFavoravel,
				Anchors: []CreateAnchorRequest{{Tipo: AnchorTipoFato, Motivo: "consta nos autos"}},
			},
			wantErr: false,
		},
		{
			name:    "missing draft_id",
			req:     CreateThesisRequest{Enunciado: "e", Forca: ForcaFavoravel},
			wantErr: true,
		},
		{
			name:    "invalid forca",
			req:     CreateThesisRequest{DraftID: "draft-1", Enunciado: "e", Forca: "inventada"},
			wantErr: true,
		},
		{
			name: "invalid nested anchor",
			req: CreateThesisRequest{
				DraftID: "draft-1", Enunciado: "e", Forca: ForcaFavoravel,
				Anchors: []CreateAnchorRequest{{Tipo: "invalido", Motivo: "x"}},
			},
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

func TestCreateSegmentRequest_Validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		req     CreateSegmentRequest
		wantErr bool
	}{
		{name: "valid", req: CreateSegmentRequest{DraftID: "d1", ThesisID: "t1", Conteudo: "texto"}, wantErr: false},
		{name: "missing conteudo", req: CreateSegmentRequest{DraftID: "d1", ThesisID: "t1"}, wantErr: true},
		{name: "missing thesis_id", req: CreateSegmentRequest{DraftID: "d1", Conteudo: "texto"}, wantErr: true},
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
