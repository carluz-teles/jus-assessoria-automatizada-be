package indexing

import (
	"crypto/sha256"
	"encoding/hex"
)

// chunk.go is the pure, DB-free chunking core of the fatia: it slices a page's extracted text
// into overlapping windows the embedder turns into vectors. It is deliberately free of any I/O
// (no storage, no pgx, no Voyage) so the boundary/overlap behavior is unit-testable in isolation
// — the pipeline (pipeline.go) composes it with the ports.

// Chunking parameters (docs/erd-documentos.md §7). A window is targetChunkSize runes wide with
// overlapRunes runes of overlap carried from the previous window (~15% of the target), so a
// sentence split across a window boundary still appears whole in one of the two chunks — the
// retrieval recall the overlap buys. Rune-based (not byte-based) so a multibyte char (acentos,
// the norm in autos) is never sliced mid-codepoint.
const (
	// targetChunkSize is the window width in runes — the middle of the ~800–1200 char band the
	// milestone specified, big enough to hold a legal argument's context, small enough to keep a
	// single embedding vector focused.
	targetChunkSize = 1000
	// overlapRunes is the runes each window carries over from the previous one — ~15% of the
	// target, the recall-vs-cost tradeoff the design picked.
	overlapRunes = 150
)

// Chunk is one indexable slice of a page: its 1-based page number, the text window, and the
// sha256 of that text (the dedup key the INSERT's ON CONFLICT targets). It is the unit the
// pipeline embeds and persists.
type Chunk struct {
	Page int
	Text string
	Hash string
}

// chunkPage slices one page's text into overlapping windows of ~targetChunkSize runes with
// ~overlapRunes of overlap, tagging each with the page number and its content hash. Empty or
// whitespace-only text yields no chunks (nothing to index). The step between window starts is
// targetChunkSize-overlapRunes, so consecutive windows share overlapRunes runes; a final short
// window (the tail shorter than a full step) is still emitted so no text is dropped.
func chunkPage(page int, text string) []Chunk {
	runes := []rune(text)
	if len(runes) == 0 {
		return nil
	}

	// step is how far the window start advances each iteration. A degenerate config (overlap >=
	// target) would make step <= 0 and loop forever; clamp it to at least 1 rune of progress.
	step := targetChunkSize - overlapRunes
	if step < 1 {
		step = 1
	}

	var chunks []Chunk
	for start := 0; start < len(runes); start += step {
		end := start + targetChunkSize
		if end > len(runes) {
			end = len(runes)
		}
		window := string(runes[start:end])
		chunks = append(chunks, Chunk{
			Page: page,
			Text: window,
			Hash: hashText(window),
		})
		// The last window reached the end of the text — stop, so a text shorter than one window
		// (or whose tail the previous step already covered) does not emit a trailing duplicate.
		if end == len(runes) {
			break
		}
	}
	return chunks
}

// chunkPages runs chunkPage over every page in document order, preserving each chunk's page
// number. It is the whole-document entry point the pipeline calls after reading the extracted
// text JSON from storage.
func chunkPages(pages []ExtractedPage) []Chunk {
	var out []Chunk
	for _, p := range pages {
		out = append(out, chunkPage(p.Page, p.Text)...)
	}
	return out
}

// hashText returns the lowercase hex sha256 of s — the chunk_hash stored on the row and the
// dedup key the ON CONFLICT (document_id, page, chunk_hash) targets. Stable across reprocesses:
// the same text always hashes the same, so a re-run inserts nothing new.
func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
