package draft

import (
	"testing"

	"github.com/google/uuid"
)

func TestCreateRequest_Validate(t *testing.T) {
	validUUID := uuid.New().String()

	tests := []struct {
		name    string
		req     CreateRequest
		wantErr bool
	}{
		{
			name:    "source required",
			req:     CreateRequest{},
			wantErr: true,
		},
		{
			name:    "source must be in closed set",
			req:     CreateRequest{Source: "unknown"},
			wantErr: true,
		},
		{
			name:    "source=intimation requires intimation_id",
			req:     CreateRequest{Source: SourceIntimation},
			wantErr: true,
		},
		{
			name:    "source=intimation with invalid UUID intimation_id is rejected",
			req:     CreateRequest{Source: SourceIntimation, IntimationID: "not-a-uuid"},
			wantErr: true,
		},
		{
			name:    "source=intimation with valid UUID is accepted",
			req:     CreateRequest{Source: SourceIntimation, IntimationID: validUUID},
			wantErr: false,
		},
		{
			name:    "source=blank with no other fields is valid",
			req:     CreateRequest{Source: SourceBlank},
			wantErr: false,
		},
		{
			name:    "source=processo with no other fields is valid",
			req:     CreateRequest{Source: SourceProcesso},
			wantErr: false,
		},
		{
			name:    "valid piece_type is accepted",
			req:     CreateRequest{Source: SourceBlank, PieceType: PieceTypeAppeal},
			wantErr: false,
		},
		{
			name:    "invalid piece_type is rejected",
			req:     CreateRequest{Source: SourceBlank, PieceType: "UNKNOWN"},
			wantErr: true,
		},
		{
			name:    "empty piece_type is accepted (inferred by domain)",
			req:     CreateRequest{Source: SourceBlank, PieceType: ""},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestChatRequest_Validate(t *testing.T) {
	tests := []struct {
		name         string
		req          ChatRequest
		wantErr      bool
		wantQuestion string // expected Question value after Validate (trimmed)
	}{
		{
			name:    "empty question is rejected",
			req:     ChatRequest{Question: ""},
			wantErr: true,
		},
		{
			name:    "whitespace-only question is rejected",
			req:     ChatRequest{Question: "   "},
			wantErr: true,
		},
		{
			name:    "tab-only question is rejected",
			req:     ChatRequest{Question: "\t\t"},
			wantErr: true,
		},
		{
			name:         "valid question is accepted",
			req:          ChatRequest{Question: "Qual o prazo recursal?"},
			wantErr:      false,
			wantQuestion: "Qual o prazo recursal?",
		},
		{
			name:         "question with surrounding spaces is accepted and trimmed",
			req:          ChatRequest{Question: "  Qual o prazo?  "},
			wantErr:      false,
			wantQuestion: "Qual o prazo?",
		},
		{
			name:    "question exceeding 2000 runes is rejected",
			req:     ChatRequest{Question: string(make([]rune, 2001))},
			wantErr: true,
		},
		{
			name:         "question of exactly 2000 runes is accepted",
			req:          ChatRequest{Question: string(make([]rune, 2000))},
			wantErr:      false,
			wantQuestion: string(make([]rune, 2000)),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr = %v", err, tt.wantErr)
			}
			if !tt.wantErr && tt.req.Question != tt.wantQuestion {
				t.Errorf("Validate() Question = %q, want %q", tt.req.Question, tt.wantQuestion)
			}
		})
	}
}

func TestInferPieceType(t *testing.T) {
	tests := []struct {
		name string
		it   *IntimationContext
		want string
	}{
		{"nil → OTHER", nil, PieceTypeOther},
		{"CITACAO → DEFENSE (cited to defend)", &IntimationContext{Type: "CITACAO"}, PieceTypeDefense},
		{
			"content 'conteste' → DEFENSE",
			&IntimationContext{Type: "INTIMACAO", Content: "Fica intimado para contestar / apresentar defesa no prazo legal."},
			PieceTypeDefense,
		},
		{
			"embargos à execução → DEFENSE",
			&IntimationContext{Type: "INTIMACAO", Class: "Execução de Título Extrajudicial", Content: "para oferecer embargos à execução"},
			PieceTypeDefense,
		},
		{
			"sentença + recurso → APPEAL",
			&IntimationContext{Type: "INTIMACAO", Content: "Publicada a sentença, fica intimado do prazo recursal para recorrer."},
			PieceTypeAppeal,
		},
		{
			"exequente intimada a indicar bens → MOTION (não DEFENSE)",
			&IntimationContext{Type: "INTIMACAO", Class: "Execução de Título Extrajudicial", Content: "<p>Fica a exequente intimada a indicar bens à penhora, no prazo de 5 dias.</p>"},
			PieceTypeMotion,
		},
		{
			"manifeste-se → MOTION",
			&IntimationContext{Type: "INTIMACAO", Content: "Manifeste-se a parte autora sobre a petição."},
			PieceTypeMotion,
		},
		{"sem sinal → OTHER", &IntimationContext{Type: "COMUNICACAO", Content: "Comunicação de ato ordinatório."}, PieceTypeOther},
		{"vazio → OTHER", &IntimationContext{}, PieceTypeOther},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inferPieceType(tt.it)
			if got != tt.want {
				t.Errorf("inferPieceType() = %q, want %q", got, tt.want)
			}
		})
	}
}
