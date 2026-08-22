package draft

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jusassessoria/platform/internal/draft/draftdb"
)

// numeric builds a valid pgtype.Numeric from a decimal string for test input.
func numeric(t *testing.T, s string) pgtype.Numeric {
	t.Helper()
	var n pgtype.Numeric
	if err := n.Scan(s); err != nil {
		t.Fatalf("scan numeric %q: %v", s, err)
	}
	return n
}

func pgUUID(t *testing.T) pgtype.UUID {
	t.Helper()
	return pgtype.UUID{Bytes: uuid.New(), Valid: true}
}

// TestDetailViewFromRow_Process asserts the read model projects the process context
// including the new autor/réu/valor-da-causa fields, and that a NULL claim_value and
// empty party arrays normalize to "" and [] (never nil) rather than surprising the FE.
func TestDetailViewFromRow_Process(t *testing.T) {
	strptr := func(s string) *string { return &s }

	tests := []struct {
		name           string
		row            draftdb.GetDraftDetailRow
		wantProcessNil bool
		wantClaimValue string
		wantPlaintiffs []string
		wantDefendants []string
	}{
		{
			name: "full process with parties and claim value",
			row: draftdb.GetDraftDetailRow{
				ID:                   uuid.New(),
				PieceType:            "DEFENSE",
				Title:                "Defesa",
				ProcessCourtRecordID: pgUUID(t),
				ProcessCaseID:        pgUUID(t),
				ProcessCnjNumber:     strptr("0001234-56.2024.8.26.0196"),
				ProcessClaimValue:    numeric(t, "15000.00"),
				ProcessPlaintiffs:    []string{"Banco Alfa S.A."},
				ProcessDefendants:    []string{"Maria Souza", "Prolheti & Marcondes LTDA"},
			},
			wantClaimValue: "15000.00",
			wantPlaintiffs: []string{"Banco Alfa S.A."},
			wantDefendants: []string{"Maria Souza", "Prolheti & Marcondes LTDA"},
		},
		{
			name: "process with NULL claim value and no parties yields empty, not nil",
			row: draftdb.GetDraftDetailRow{
				ID:                   uuid.New(),
				PieceType:            "DEFENSE",
				Title:                "Defesa",
				ProcessCourtRecordID: pgUUID(t),
				ProcessCaseID:        pgUUID(t),
				ProcessClaimValue:    pgtype.Numeric{Valid: false},
				ProcessPlaintiffs:    nil,
				ProcessDefendants:    nil,
			},
			wantClaimValue: "",
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
			if view.Process.ClaimValue != tt.wantClaimValue {
				t.Errorf("ClaimValue = %q, want %q", view.Process.ClaimValue, tt.wantClaimValue)
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

// TestDetailToResponse_ProcessParties asserts the HTTP response carries the parties
// and valor da causa, and that empty slices serialize as [] (non-nil) — the FE maps
// over them unconditionally.
func TestDetailToResponse_ProcessParties(t *testing.T) {
	view := &DraftDetailView{
		ID:        uuid.NewString(),
		PieceType: "DEFENSE",
		Title:     "Defesa",
		Process: &ProcessView{
			CaseID:     uuid.NewString(),
			ClaimValue: "15000.00",
			Plaintiffs: []string{"Banco Alfa S.A."},
			Defendants: []string{},
		},
	}

	resp := detailToResponse(view)
	if resp.Process == nil {
		t.Fatal("expected non-nil process in response")
	}
	if resp.Process.ClaimValue != "15000.00" {
		t.Errorf("claim_value = %q, want 15000.00", resp.Process.ClaimValue)
	}
	if len(resp.Process.Plaintiffs) != 1 || resp.Process.Plaintiffs[0] != "Banco Alfa S.A." {
		t.Errorf("plaintiffs = %v", resp.Process.Plaintiffs)
	}
	if resp.Process.Defendants == nil {
		t.Error("defendants is nil, want [] so JSON is not null")
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
