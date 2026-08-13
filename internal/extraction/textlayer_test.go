package extraction

import "testing"

// TestHasUsableTextLayer exercises the density rule that routes a stamped scan to OCR while
// keeping a real text document's text layer. The pure helper is tested directly (no PDF
// fixture): the decision is total/numPages >= minCharsPerPage, guarding a 0-page document.
func TestHasUsableTextLayer(t *testing.T) {
	tests := []struct {
		name     string
		total    int
		numPages int
		want     bool
	}{
		{
			// The prod case: a 71-page scan carrying only a thin PJe header stamp per page.
			// ~2100 chars total defeats any fixed total floor, but ~66 chars/page is well
			// below minCharsPerPage → OCR.
			name:     "stamped scan (66 chars/page) → no text layer",
			total:    66 * 71,
			numPages: 71,
			want:     false,
		},
		{
			name:     "dense text document (800 chars/page) → has text layer",
			total:    800 * 40,
			numPages: 40,
			want:     true,
		},
		{
			name:     "exactly at the floor → has text layer",
			total:    minCharsPerPage * 3,
			numPages: 3,
			want:     true,
		},
		{
			name:     "one below the floor → no text layer",
			total:    minCharsPerPage*3 - 1,
			numPages: 3,
			want:     false,
		},
		{
			name:     "zero pages → no text layer (guards the division)",
			total:    500,
			numPages: 0,
			want:     false,
		},
		{
			name:     "empty scan (no text at all) → no text layer",
			total:    0,
			numPages: 10,
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasUsableTextLayer(tt.total, tt.numPages); got != tt.want {
				t.Errorf("hasUsableTextLayer(%d, %d) = %v, want %v", tt.total, tt.numPages, got, tt.want)
			}
		})
	}
}
