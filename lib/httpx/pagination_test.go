package httpx_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/httpx"
)

func TestPageMeta_MarshalsTotals(t *testing.T) {
	t.Parallel()

	cursor := "eyJsYXN0X2lkIjoiMSJ9"
	page := httpx.Page[int]{
		Data: []int{1, 2},
		Page: httpx.PageMeta{
			NextCursor: &cursor,
			Limit:      25,
			TotalCount: 32,   // current context (filtered when a search is active)
			Total:      1247, // tenant-wide, always
		},
	}

	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	got := string(raw)
	for _, want := range []string{
		`"next_cursor":"eyJsYXN0X2lkIjoiMSJ9"`,
		`"limit":25`,
		`"total_count":32`,
		`"total":1247`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("marshaled page is missing %s\ngot: %s", want, got)
		}
	}
}

// The envelope carries a filters block on every page. With no selectable options it
// must serialize as {} (never null) — the FE always treats filters as an object.
func TestPage_FiltersAlwaysObject(t *testing.T) {
	t.Parallel()

	page := httpx.Page[int]{
		Data:    []int{1, 2},
		Page:    httpx.PageMeta{Limit: 20},
		Filters: httpx.Filters{}.NonNil(),
	}

	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"filters":{}`) {
		t.Errorf("envelope must serialize filters as {} when empty\ngot: %s", raw)
	}
}

func TestCursor_RoundTrips(t *testing.T) {
	t.Parallel()

	want := httpx.Cursor{LastID: "018f-uuid-v7", LastSortValue: "2026-07-28T12:00:00Z"}

	got, err := httpx.DecodeCursor(httpx.EncodeCursor(want))
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if got != want {
		t.Errorf("round-trip = %+v, want %+v", got, want)
	}
}

func TestDecodeCursor_InvalidIsApperrInvalid(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		token string
	}{
		{name: "not base64", token: "garbage!!!"},
		{name: "base64 but not json", token: "Zm9vYmFy"}, // "foobar"
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := httpx.DecodeCursor(tt.token)
			if err == nil {
				t.Fatal("expected an error for an invalid cursor")
			}

			ae, ok := apperr.From(err)
			if !ok {
				t.Fatalf("error is not an *apperr.AppError: %v", err)
			}
			if ae.Kind != apperr.KindInvalid {
				t.Errorf("kind = %q, want %q", ae.Kind, apperr.KindInvalid)
			}
		})
	}
}

func TestClampLimit(t *testing.T) {
	t.Parallel()

	const (
		def = 20
		max = 100
	)

	tests := []struct {
		name      string
		requested int
		want      int
	}{
		{name: "zero falls back to default", requested: 0, want: def},
		{name: "negative falls back to default", requested: -5, want: def},
		{name: "within range is kept", requested: 50, want: 50},
		{name: "at max is kept", requested: max, want: max},
		{name: "above max is capped", requested: 500, want: max},
		{name: "one is kept", requested: 1, want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := httpx.ClampLimit(tt.requested, def, max); got != tt.want {
				t.Errorf("ClampLimit(%d, %d, %d) = %d, want %d", tt.requested, def, max, got, tt.want)
			}
		})
	}
}
