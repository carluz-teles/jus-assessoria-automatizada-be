package acquisition

import "testing"

// deriveWorkStage é projeção pura (prazo + peça) — cobre a precedência marco-mais-
// avançado-vence e as fronteiras entre etapas.
func TestDeriveWorkStage(t *testing.T) {
	t.Parallel()

	confirmado := &IntimacaoPrazoView{Confirmed: true}
	naoConfirmado := &IntimacaoPrazoView{Confirmed: false}

	tests := []struct {
		name        string
		prazo       *IntimacaoPrazoView
		draftStatus string
		draftFiled  bool
		want        string
	}{
		{name: "sem prazo e sem peça", prazo: nil, want: WorkStageReceived},
		{name: "prazo derivado nao confirmado", prazo: naoConfirmado, want: WorkStageAwaitingConfirmation},
		{name: "prazo confirmado sem peça", prazo: confirmado, want: WorkStageConfirmed},
		{name: "peça em elaboração", prazo: confirmado, draftStatus: "DRAFT", want: WorkStageDrafting},
		{name: "peça em revisão do sócio (REVIEWED)", prazo: confirmado, draftStatus: "REVIEWED", want: WorkStagePartnerReview},
		{name: "peça assinada (SIGNED)", prazo: confirmado, draftStatus: "SIGNED", want: WorkStagePartnerReview},
		{name: "peça protocolada vence sobre status", prazo: confirmado, draftStatus: "SIGNED", draftFiled: true, want: WorkStageFiled},
		{name: "peça protocolada mesmo sem prazo confirmado", prazo: naoConfirmado, draftStatus: "DRAFT", draftFiled: true, want: WorkStageFiled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := deriveWorkStage(tt.prazo, tt.draftStatus, tt.draftFiled)
			if got != tt.want {
				t.Errorf("deriveWorkStage(%v, %q, %v) = %q, want %q", tt.prazo, tt.draftStatus, tt.draftFiled, got, tt.want)
			}
		})
	}
}
