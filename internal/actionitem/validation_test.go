package actionitem

import "testing"

// TestDeriveTipoOrigemStatus is the motor de precedência unit (docs §3): a teor that
// declares the tipo is trusted outright (declarado/confiável); anything else is the
// classifier's inference and is born a_confirmar — the piso that never lets an IA guess
// become confiável on its own.
func TestDeriveTipoOrigemStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		declarado      bool
		wantOrigem     TipoOrigem
		wantTipoStatus TipoStatus
	}{
		{name: "declarado is confiavel", declarado: true, wantOrigem: TipoOrigemDeclarado, wantTipoStatus: TipoStatusConfiavel},
		{name: "inferido is a_confirmar", declarado: false, wantOrigem: TipoOrigemIA, wantTipoStatus: TipoStatusAConfirmar},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotOrigem, gotStatus := deriveTipoOrigemStatus(tt.declarado)
			if gotOrigem != tt.wantOrigem {
				t.Errorf("tipo_origem = %q, want %q", gotOrigem, tt.wantOrigem)
			}
			if gotStatus != tt.wantTipoStatus {
				t.Errorf("tipo_status = %q, want %q", gotStatus, tt.wantTipoStatus)
			}
		})
	}
}

// TestSanitizeCandidate covers the viés seguro degrade: an unknown tipo falls back to
// ciência (no peça, ever); gera_peca without a KNOWN piece_profile_key is stripped rather
// than risking an FK violation; a valid combination passes through untouched.
func TestSanitizeCandidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                string
		tipo                string
		geraPeca            bool
		pieceProfileKey     string
		wantTipo            string
		wantGeraPeca        bool
		wantPieceProfileKey string
	}{
		{
			name: "valid contestacao passes through", tipo: TipoContestar, geraPeca: true, pieceProfileKey: "contestacao",
			wantTipo: TipoContestar, wantGeraPeca: true, wantPieceProfileKey: "contestacao",
		},
		{
			name: "ciencia never carries a profile", tipo: TipoCiencia, geraPeca: false, pieceProfileKey: "",
			wantTipo: TipoCiencia, wantGeraPeca: false, wantPieceProfileKey: "",
		},
		{
			name: "unknown tipo degrades to ciencia", tipo: "invalido", geraPeca: true, pieceProfileKey: "contestacao",
			wantTipo: TipoCiencia, wantGeraPeca: false, wantPieceProfileKey: "",
		},
		{
			name: "gera_peca with unknown profile key strips the peça", tipo: TipoRecorrer, geraPeca: true, pieceProfileKey: "peca_inexistente",
			wantTipo: TipoRecorrer, wantGeraPeca: false, wantPieceProfileKey: "",
		},
		{
			name: "gera_peca false ignores a stray profile key", tipo: TipoManifestar, geraPeca: false, pieceProfileKey: "contestacao",
			wantTipo: TipoManifestar, wantGeraPeca: false, wantPieceProfileKey: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotTipo, gotGeraPeca, gotKey := sanitizeCandidate(tt.tipo, tt.geraPeca, tt.pieceProfileKey)
			if gotTipo != tt.wantTipo || gotGeraPeca != tt.wantGeraPeca || gotKey != tt.wantPieceProfileKey {
				t.Errorf("sanitizeCandidate(%q, %v, %q) = (%q, %v, %q), want (%q, %v, %q)",
					tt.tipo, tt.geraPeca, tt.pieceProfileKey,
					gotTipo, gotGeraPeca, gotKey,
					tt.wantTipo, tt.wantGeraPeca, tt.wantPieceProfileKey)
			}
		})
	}
}

// TestActionItem_Validate covers the belt-and-suspenders checks that mirror migration
// 0086's CHECK constraints.
func TestActionItem_Validate(t *testing.T) {
	t.Parallel()

	base := ActionItem{
		Tipo: TipoContestar, TipoOrigem: TipoOrigemDeclarado, TipoStatus: TipoStatusConfiavel,
	}

	tests := []struct {
		name    string
		mutate  func(a ActionItem) ActionItem
		wantErr bool
	}{
		{name: "valid declarado, no peça", mutate: func(a ActionItem) ActionItem { return a }, wantErr: false},
		{
			name: "valid gera_peca with profile",
			mutate: func(a ActionItem) ActionItem {
				a.GeraPeca = true
				a.PieceProfileKey = "contestacao"
				return a
			},
			wantErr: false,
		},
		{
			name: "gera_peca without profile is invalid",
			mutate: func(a ActionItem) ActionItem {
				a.GeraPeca = true
				return a
			},
			wantErr: true,
		},
		{
			name: "profile without gera_peca is invalid",
			mutate: func(a ActionItem) ActionItem {
				a.PieceProfileKey = "contestacao"
				return a
			},
			wantErr: true,
		},
		{
			name: "confianca without ia origin is invalid",
			mutate: func(a ActionItem) ActionItem {
				c := 0.5
				a.Confianca = &c
				return a
			},
			wantErr: true,
		},
		{
			name: "confianca with ia origin is valid",
			mutate: func(a ActionItem) ActionItem {
				a.TipoOrigem = TipoOrigemIA
				a.TipoStatus = TipoStatusAConfirmar
				c := 0.5
				a.Confianca = &c
				return a
			},
			wantErr: false,
		},
		{
			name:    "invalid tipo_origem",
			mutate:  func(a ActionItem) ActionItem { a.TipoOrigem = "bogus"; return a },
			wantErr: true,
		},
		{
			name:    "invalid tipo_status",
			mutate:  func(a ActionItem) ActionItem { a.TipoStatus = "bogus"; return a },
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			item := tt.mutate(base)
			err := item.validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
