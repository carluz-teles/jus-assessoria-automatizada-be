package acquisition

import (
	"context"
	"fmt"
)

// parser_multiplex.go composes several source-specific parsers behind the single
// Parser port the sync use case consumes. The use case fetches a payload and
// hands it to ONE parser; with more than one source live (the real DJEN plus the
// DATAJUD stub, which speak different formats) the payload must be routed to the
// member that understands it. That is exactly what the Parser port's CanParse
// method is for — MultiParser is the composition-root adapter that uses it.

// MultiParser routes a RawPayload to the first member that CanParse it. It is a
// Parser itself, so it drops into NewSyncUseCase unchanged; the composition root
// (cmd/worker-ingestao) builds it from the concrete parsers it wires.
type MultiParser struct {
	parsers []Parser
}

var _ Parser = (*MultiParser)(nil)

// NewMultiParser composes parsers in priority order: the first member whose
// CanParse returns true for a payload wins. Callers list the more specific parser
// first when two could claim the same payload.
func NewMultiParser(parsers ...Parser) *MultiParser {
	return &MultiParser{parsers: parsers}
}

// CanParse reports whether any member understands the payload.
func (m *MultiParser) CanParse(p RawPayload) bool {
	for _, parser := range m.parsers {
		if parser.CanParse(p) {
			return true
		}
	}
	return false
}

// Parse delegates to the first member that CanParse the payload. When none does,
// it returns ErrNoParserForPayload — a wiring bug (a source has a connector but no
// parser) that the sync use case surfaces as a terminal parse failure.
func (m *MultiParser) Parse(ctx context.Context, p RawPayload) (ParsedResult, error) {
	for _, parser := range m.parsers {
		if parser.CanParse(p) {
			return parser.Parse(ctx, p)
		}
	}
	return ParsedResult{}, fmt.Errorf("djen parser multiplex: source %q connector %q: %w",
		p.Source, p.ConnectorID, ErrNoParserForPayload)
}
