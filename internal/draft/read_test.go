package draft

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jusassessoria/platform/internal/draft/draftdb"
)

func pgUUID(t *testing.T) pgtype.UUID {
	t.Helper()
	return pgtype.UUID{Bytes: uuid.New(), Valid: true}
}

// TestDetailViewFromRow_Process asserts the read model projects the process context
// including the autor/réu fields, and that empty party arrays normalize to [] (never
// nil) rather than surprising the FE.
func TestDetailViewFromRow_Process(t *testing.T) {
	strptr := func(s string) *string { return &s }

	tests := []struct {
		name           string
		row            draftdb.GetDraftDetailRow
		wantProcessNil bool
		wantPlaintiffs []string
		wantDefendants []string
	}{
		{
			name: "full process with parties",
			row: draftdb.GetDraftDetailRow{
				ID:                   uuid.New(),
				PieceType:            "DEFENSE",
				Title:                "Defesa",
				ProcessCourtRecordID: pgUUID(t),
				ProcessCaseID:        pgUUID(t),
				ProcessCnjNumber:     strptr("0001234-56.2024.8.26.0196"),
				ProcessPlaintiffs:    []string{"Banco Alfa S.A."},
				ProcessDefendants:    []string{"Maria Souza", "Prolheti & Marcondes LTDA"},
			},
			wantPlaintiffs: []string{"Banco Alfa S.A."},
			wantDefendants: []string{"Maria Souza", "Prolheti & Marcondes LTDA"},
		},
		{
			name: "process with no parties yields empty, not nil",
			row: draftdb.GetDraftDetailRow{
				ID:                   uuid.New(),
				PieceType:            "DEFENSE",
				Title:                "Defesa",
				ProcessCourtRecordID: pgUUID(t),
				ProcessCaseID:        pgUUID(t),
				ProcessPlaintiffs:    nil,
				ProcessDefendants:    nil,
			},
			wantPlaintiffs: []string{},
			wantDefendants: []string{},
		},
		{
			name: "blank draft has no process block",
			row: draftdb.GetDraftDetailRow{
				ID:        uuid.New(),
				PieceType: "BLANK",
				Title:     "Rascunho",
				// ProcessCourtRecordID left as zero (Valid=false) → no process.
			},
			wantProcessNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			view := detailViewFromRow(tt.row)

			if tt.wantProcessNil {
				if view.Process != nil {
					t.Fatalf("expected nil Process, got %+v", view.Process)
				}
				return
			}

			if view.Process == nil {
				t.Fatal("expected non-nil Process")
			}
			if view.Process.Plaintiffs == nil {
				t.Error("Plaintiffs is nil, want non-nil slice")
			}
			if view.Process.Defendants == nil {
				t.Error("Defendants is nil, want non-nil slice")
			}
			if got := view.Process.Plaintiffs; !equalStrings(got, tt.wantPlaintiffs) {
				t.Errorf("Plaintiffs = %v, want %v", got, tt.wantPlaintiffs)
			}
			if got := view.Process.Defendants; !equalStrings(got, tt.wantDefendants) {
				t.Errorf("Defendants = %v, want %v", got, tt.wantDefendants)
			}
		})
	}
}

// TestDetailToResponse_ProcessParties asserts the HTTP response carries the parties,
// and that empty slices serialize as [] (non-nil) — the FE maps over them
// unconditionally.
func TestDetailToResponse_ProcessParties(t *testing.T) {
	view := &DraftDetailView{
		ID:        uuid.NewString(),
		PieceType: "DEFENSE",
		Title:     "Defesa",
		Process: &ProcessView{
			CaseID:     uuid.NewString(),
			Plaintiffs: []string{"Banco Alfa S.A."},
			Defendants: []string{},
		},
	}

	resp := detailToResponse(view)
	if resp.Process == nil {
		t.Fatal("expected non-nil process in response")
	}
	if len(resp.Process.Plaintiffs) != 1 || resp.Process.Plaintiffs[0] != "Banco Alfa S.A." {
		t.Errorf("plaintiffs = %v", resp.Process.Plaintiffs)
	}
	if resp.Process.Defendants == nil {
		t.Error("defendants is nil, want [] so JSON is not null")
	}
}

// TestDetailToResponse_PartyIsClient asserts is_client rides through the read model to
// the wire: the flagged party serializes is_client=true, the other false, and the key
// is always present (json.Marshal never omits a bool) so the FE stops guessing by role.
func TestDetailToResponse_PartyIsClient(t *testing.T) {
	view := &DraftDetailView{
		ID:        uuid.NewString(),
		PieceType: "DEFENSE",
		Title:     "Defesa",
		Parties: []PartyInfo{
			{Role: "DEFENDANT", Name: "CLIENTE", IsClient: true, Counsels: []PartyCounselInfo{}},
			{Role: "DEFENDANT", Name: "OUTRO", IsClient: false, Counsels: []PartyCounselInfo{}},
		},
	}

	resp := detailToResponse(view)
	if len(resp.Parties) != 2 {
		t.Fatalf("parties = %d, want 2", len(resp.Parties))
	}
	if !resp.Parties[0].IsClient {
		t.Error("first party (flagged) must be IsClient=true")
	}
	if resp.Parties[1].IsClient {
		t.Error("second party must be IsClient=false")
	}

	raw, err := json.Marshal(resp.Parties[1])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"is_client":false`)) {
		t.Errorf("is_client key must always be present in JSON, got %s", raw)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
