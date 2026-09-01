package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestPartyRoleFromPolo pins the consistency contract: eproc's polo ("AUTOR"/"REU")
// must map onto the SAME party.role enum DJEN writes (PLAINTIFF/DEFENDANT) so an eproc
// party merges into the DJEN-created row (shared unique key), never a duplicate under a
// different role. An unrecognized polo is skipped (ok=false), not written blindly.
func TestPartyRoleFromPolo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		polo     string
		wantRole string
		wantOK   bool
	}{
		{name: "autor maps to plaintiff", polo: "AUTOR", wantRole: "PLAINTIFF", wantOK: true},
		{name: "reu maps to defendant", polo: "REU", wantRole: "DEFENDANT", wantOK: true},
		{name: "lowercase is normalized", polo: "autor", wantRole: "PLAINTIFF", wantOK: true},
		{name: "surrounding space is trimmed", polo: "  REU  ", wantRole: "DEFENDANT", wantOK: true},
		{name: "unknown polo is skipped", polo: "TERCEIRO", wantRole: "", wantOK: false},
		{name: "empty polo is skipped", polo: "", wantRole: "", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			is := assert.New(t)
			role, ok := partyRoleFromPolo(tt.polo)
			is.Equal(tt.wantOK, ok)
			is.Equal(tt.wantRole, role)
		})
	}
}
