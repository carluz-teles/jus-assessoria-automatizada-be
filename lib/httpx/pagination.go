package httpx

import (
	"encoding/base64"
	"encoding/json"

	"github.com/jusassessoria/platform/lib/apperr"
)

// Pagination defaults (docs/erd-backend.md §4e.2). ClampLimit enforces them so a
// client can neither disable paging (0) nor request an unbounded page.
const (
	DefaultLimit = 20
	MaxLimit     = 100
)

// PageMeta is the pagination block of the list envelope. NextCursor is nil on
// the last page. The two totals drive the "X de Y" counter on cursor-paginated
// screens: TotalCount is the current context (filtered by ?search when present),
// Total is the tenant-wide count regardless of any filter. With no search active
// they are equal, so a single COUNT(*) fills both.
type PageMeta struct {
	NextCursor *string `json:"next_cursor"`
	Limit      int     `json:"limit"`
	TotalCount int64   `json:"total_count"`
	Total      int64   `json:"total"`
}

// Page is the cursor-paginated list envelope (§4e.2): {data, page:{...}, filters}.
// It is generic so every slice reuses the same shape with its own read-model DTO.
// Filters is the selectable-options block for the list's filter chips: always present,
// {} (never null) when the list has no filters. The page builders set it from the
// Result's Filters (NonNil), so an empty envelope still serializes an object.
type Page[T any] struct {
	Data    []T      `json:"data"`
	Page    PageMeta `json:"page"`
	Filters Filters  `json:"filters"`
}

// Cursor is the opaque, stateless pagination cursor. It carries the last row's
// id and sort value so the next query resumes with a keyset predicate — no
// offset, stable under concurrent inserts.
type Cursor struct {
	LastID        string `json:"last_id"`
	LastSortValue string `json:"last_sort_value"`
}

// EncodeCursor serializes a Cursor to a URL-safe opaque token. The client treats
// it as opaque and echoes it back verbatim on the next request.
func EncodeCursor(cur Cursor) string {
	// A struct of two strings cannot fail to marshal, so the error is ignored.
	raw, _ := json.Marshal(cur)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// DecodeCursor parses a token produced by EncodeCursor. A malformed token is a
// bad query param — a client error — so it maps to apperr.KindInvalid (→ 400),
// never a 500.
func DecodeCursor(token string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return Cursor{}, apperr.NewInvalid("invalid cursor")
	}

	var cur Cursor
	if err := json.Unmarshal(raw, &cur); err != nil {
		return Cursor{}, apperr.NewInvalid("invalid cursor")
	}

	return cur, nil
}

// ClampLimit normalizes a client-requested page size: non-positive falls back to
// def, anything above max is capped at max.
func ClampLimit(requested, def, max int) int {
	if requested <= 0 {
		return def
	}
	if requested > max {
		return max
	}
	return requested
}
