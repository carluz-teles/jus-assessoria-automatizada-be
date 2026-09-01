package compliancerule

import "testing"

func TestCreateRuleRequest_Validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		req     CreateRuleRequest
		wantErr bool
	}{
		{
			name: "valid",
			req: CreateRuleRequest{
				Key: "pedido_certo", Descricao: "pedido certo e determinado",
				Severidade: string(SeveridadeBloqueante), Verificacao: string(VerificacaoDeterministica),
			},
			wantErr: false,
		},
		{
			name:    "invalid severidade",
			req:     CreateRuleRequest{Key: "x", Descricao: "d", Severidade: "critica", Verificacao: string(VerificacaoDeterministica)},
			wantErr: true,
		},
		{
			name:    "invalid verificacao",
			req:     CreateRuleRequest{Key: "x", Descricao: "d", Severidade: string(SeveridadeAviso), Verificacao: "manual"},
			wantErr: true,
		},
		{
			name:    "missing key",
			req:     CreateRuleRequest{Descricao: "d", Severidade: string(SeveridadeAviso), Verificacao: string(VerificacaoDeterministica)},
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

func TestAddRuleToProfileRequest_Validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		req     AddRuleToProfileRequest
		wantErr bool
	}{
		{name: "valid without override", req: AddRuleToProfileRequest{RuleKey: "pedido_certo"}, wantErr: false},
		{name: "valid with override", req: AddRuleToProfileRequest{RuleKey: "pedido_certo", OverrideSeveridade: string(SeveridadeAviso)}, wantErr: false},
		{name: "missing rule_key", req: AddRuleToProfileRequest{}, wantErr: true},
		{name: "invalid override", req: AddRuleToProfileRequest{RuleKey: "x", OverrideSeveridade: "critica"}, wantErr: true},
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
