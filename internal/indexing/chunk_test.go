package indexing

import (
	"strings"
	"testing"
)

// chunk_test.go covers the pure chunker: boundaries (short text → one chunk), the overlap
// invariant (consecutive windows share overlapRunes runes), the page tag, the hash, and the
// empty/whitespace edge (no chunks). No I/O — the chunker is DB/storage/embedder-free.

func TestChunkPage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		page       int
		text       string
		wantChunks int
		checkPages bool
	}{
		{
			name:       "empty text yields no chunks",
			page:       1,
			text:       "",
			wantChunks: 0,
		},
		{
			name:       "text shorter than one window is a single chunk",
			page:       3,
			text:       strings.Repeat("a", 500),
			wantChunks: 1,
			checkPages: true,
		},
		{
			name:       "text exactly one window is a single chunk",
			page:       1,
			text:       strings.Repeat("a", targetChunkSize),
			wantChunks: 1,
		},
		{
			// step = targetChunkSize - overlapRunes = 850. len = 1000 + 850 = 1850 → starts at 0 and
			// 850; the 850 window ends at 1850 = len, so exactly 2 chunks.
			name:       "text of one window plus one step is two chunks",
			page:       2,
			text:       strings.Repeat("b", targetChunkSize+(targetChunkSize-overlapRunes)),
			wantChunks: 2,
			checkPages: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := chunkPage(tt.page, tt.text)
			if len(got) != tt.wantChunks {
				t.Fatalf("chunkPage() = %d chunks, want %d", len(got), tt.wantChunks)
			}
			if tt.checkPages {
				for i, c := range got {
					if c.Page != tt.page {
						t.Errorf("chunk %d page = %d, want %d", i, c.Page, tt.page)
					}
					if c.Hash == "" {
						t.Errorf("chunk %d has empty hash", i)
					}
					if c.Hash != hashText(c.Text) {
						t.Errorf("chunk %d hash mismatch", i)
					}
				}
			}
		})
	}
}

// TestChunkPageOverlap asserts the overlap invariant: consecutive windows share exactly
// overlapRunes runes (the tail of chunk i is the head of chunk i+1).
func TestChunkPageOverlap(t *testing.T) {
	t.Parallel()

	// A distinct run of runes so overlap is visually checkable: 2500 unique-ish chars.
	var b strings.Builder
	for i := 0; i < 2500; i++ {
		b.WriteByte(byte('a' + (i % 26)))
	}
	text := b.String()

	chunks := chunkPage(1, text)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}

	for i := 0; i+1 < len(chunks); i++ {
		cur := []rune(chunks[i].Text)
		next := []rune(chunks[i+1].Text)
		// The last overlapRunes of cur must equal the first overlapRunes of next.
		if len(cur) < overlapRunes || len(next) < overlapRunes {
			continue // a short tail window: overlap invariant only applies to full windows
		}
		tail := string(cur[len(cur)-overlapRunes:])
		head := string(next[:overlapRunes])
		if tail != head {
			t.Errorf("chunk %d/%d overlap mismatch:\n tail=%q\n head=%q", i, i+1, tail, head)
		}
	}
}

// TestChunkPageWindowSize asserts no window exceeds targetChunkSize runes.
func TestChunkPageWindowSize(t *testing.T) {
	t.Parallel()

	text := strings.Repeat("x", 5000)
	for _, c := range chunkPage(1, text) {
		if n := len([]rune(c.Text)); n > targetChunkSize {
			t.Errorf("window of %d runes exceeds target %d", n, targetChunkSize)
		}
	}
}

// TestChunkPagesPreservesPageNumbers asserts chunkPages keeps each chunk on its source page.
func TestChunkPagesPreservesPageNumbers(t *testing.T) {
	t.Parallel()

	pages := []ExtractedPage{
		{Page: 1, Text: strings.Repeat("a", 400)},
		{Page: 2, Text: strings.Repeat("b", 400)},
		{Page: 5, Text: ""}, // empty page contributes nothing
		{Page: 7, Text: strings.Repeat("c", 400)},
	}
	got := chunkPages(pages)
	if len(got) != 3 {
		t.Fatalf("chunkPages() = %d chunks, want 3", len(got))
	}
	wantPages := []int{1, 2, 7}
	for i, c := range got {
		if c.Page != wantPages[i] {
			t.Errorf("chunk %d page = %d, want %d", i, c.Page, wantPages[i])
		}
	}
}
