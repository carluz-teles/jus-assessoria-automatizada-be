package actionitem

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/lib/events"
)

// TestIntimationAnalyzed_ContractRoundTrip is the producer∥consumer contract guard
// (memória parallel-producer-consumer-roundtrip): it MARSHALS the producer's
// acquisition.IntimationAnalyzed and UNMARSHALS it into this slice's LOCAL decode struct,
// asserting every field OnIntimationAnalyzed reads survives the wire. This slice
// deliberately does NOT import the producer's struct (only the type const), so without
// this test a field rename on either side would drift silently and materialization would
// derive from zero values. It also pins the shared dotted id.
func TestIntimationAnalyzed_ContractRoundTrip(t *testing.T) {
	if TypeIntimationAnalyzed != acquisition.TypeIntimationAnalyzed {
		t.Fatalf("consumed type %q != producer type %q", TypeIntimationAnalyzed, acquisition.TypeIntimationAnalyzed)
	}

	profileKey := "contestacao"
	confianca := 0.42
	producer := acquisition.IntimationAnalyzed{
		Base:          events.Base{EventID: uuid.NewString(), Aggregate: uuid.NewString()},
		TenantID:      uuid.NewString(),
		IntimationID:  uuid.NewString(),
		CourtRecordID: uuid.NewString(),
		DeadlineID:    uuid.NewString(),
		Providencias: []acquisition.ProvidenciaCandidate{
			{Title: "Contestar a ação", Description: "Prazo de 15 dias", Tipo: "contestar", GeraPeca: true, PieceProfileKey: &profileKey, Declarado: true},
			{Title: "Manifestar-se", Description: "", Tipo: "manifestar", GeraPeca: false, PieceProfileKey: nil, Declarado: false, Confianca: &confianca},
		},
	}

	raw, err := json.Marshal(producer)
	if err != nil {
		t.Fatalf("marshal producer: %v", err)
	}

	var got IntimationAnalyzed
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal into local shape: %v", err)
	}

	if got.EventID != producer.EventID {
		t.Errorf("EventID = %q, want %q", got.EventID, producer.EventID)
	}
	if got.TenantID != producer.TenantID {
		t.Errorf("TenantID = %q, want %q", got.TenantID, producer.TenantID)
	}
	if got.IntimationID != producer.IntimationID {
		t.Errorf("IntimationID = %q, want %q", got.IntimationID, producer.IntimationID)
	}
	if got.CourtRecordID != producer.CourtRecordID {
		t.Errorf("CourtRecordID = %q, want %q", got.CourtRecordID, producer.CourtRecordID)
	}
	if got.DeadlineID != producer.DeadlineID {
		t.Errorf("DeadlineID = %q, want %q", got.DeadlineID, producer.DeadlineID)
	}
	if len(got.Providencias) != len(producer.Providencias) {
		t.Fatalf("Providencias len = %d, want %d", len(got.Providencias), len(producer.Providencias))
	}
	for i, want := range producer.Providencias {
		got := got.Providencias[i]
		if got.Title != want.Title {
			t.Errorf("Providencias[%d].Title = %q, want %q", i, got.Title, want.Title)
		}
		if got.Description != want.Description {
			t.Errorf("Providencias[%d].Description = %q, want %q", i, got.Description, want.Description)
		}
		if got.Tipo != want.Tipo {
			t.Errorf("Providencias[%d].Tipo = %q, want %q", i, got.Tipo, want.Tipo)
		}
		if got.GeraPeca != want.GeraPeca {
			t.Errorf("Providencias[%d].GeraPeca = %v, want %v", i, got.GeraPeca, want.GeraPeca)
		}
		if (got.PieceProfileKey == nil) != (want.PieceProfileKey == nil) {
			t.Errorf("Providencias[%d].PieceProfileKey nil-ness mismatch: got %v, want %v", i, got.PieceProfileKey, want.PieceProfileKey)
		} else if got.PieceProfileKey != nil && *got.PieceProfileKey != *want.PieceProfileKey {
			t.Errorf("Providencias[%d].PieceProfileKey = %q, want %q", i, *got.PieceProfileKey, *want.PieceProfileKey)
		}
		if got.Declarado != want.Declarado {
			t.Errorf("Providencias[%d].Declarado = %v, want %v", i, got.Declarado, want.Declarado)
		}
		if (got.Confianca == nil) != (want.Confianca == nil) {
			t.Errorf("Providencias[%d].Confianca nil-ness mismatch: got %v, want %v", i, got.Confianca, want.Confianca)
		} else if got.Confianca != nil && *got.Confianca != *want.Confianca {
			t.Errorf("Providencias[%d].Confianca = %v, want %v", i, *got.Confianca, *want.Confianca)
		}
	}
}

// TestActionItemEvents_ImplementEvent pins the produced events' Type()/AggregateType()
// wiring — a copy-paste mistake across the three (created/confirmed/discarded) would
// otherwise only surface at runtime via the outbox row's type column.
func TestActionItemEvents_ImplementEvent(t *testing.T) {
	t.Parallel()

	item := &ActionItem{ID: "a1", TenantID: "t1", IntimationID: "i1", Tipo: "contestar"}

	tests := []struct {
		name       string
		ev         events.Event
		wantType   string
		wantAggreg string
	}{
		{name: "created", ev: newActionItemCreated(item), wantType: TypeActionItemCreated, wantAggreg: "action_item"},
		{name: "confirmed", ev: newActionItemConfirmed(item), wantType: TypeActionItemConfirmed, wantAggreg: "action_item"},
		{name: "discarded", ev: newActionItemDiscarded(item), wantType: TypeActionItemDiscarded, wantAggreg: "action_item"},
		{name: "reclassified", ev: newActionItemReclassified(item), wantType: TypeActionItemReclassified, wantAggreg: "action_item"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.ev.Type() != tt.wantType {
				t.Errorf("Type() = %q, want %q", tt.ev.Type(), tt.wantType)
			}
			if tt.ev.AggregateType() != tt.wantAggreg {
				t.Errorf("AggregateType() = %q, want %q", tt.ev.AggregateType(), tt.wantAggreg)
			}
			if tt.ev.AggregateID() != item.ID {
				t.Errorf("AggregateID() = %q, want %q", tt.ev.AggregateID(), item.ID)
			}
			if tt.ev.IdempotencyKey() == "" {
				t.Error("IdempotencyKey() = empty, want a fresh event id")
			}
		})
	}
}
