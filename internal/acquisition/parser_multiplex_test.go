package acquisition

import (
	"errors"
	"testing"
)

// predicateParser is a fake Parser that claims a payload by a caller-supplied
// predicate and tags its result so the test can assert which member ran. It is
// distinct from sync_test.go's stubParser (which claims everything).
type predicateParser struct {
	canParse func(RawPayload) bool
	tag      string
}

func (p predicateParser) CanParse(payload RawPayload) bool { return p.canParse(payload) }

func (p predicateParser) Parse(RawPayload) (ParsedResult, error) {
	// Tag the run through the first court record's Court field.
	return ParsedResult{CourtRecords: []ParsedCourtRecord{{Court: p.tag}}}, nil
}

func bySource(source string) func(RawPayload) bool {
	return func(p RawPayload) bool { return p.Source == source }
}

// Parse routes each payload to the first member that CanParse it, in priority
// order.
func TestMultiParser_ParseRoutesToFirstMatch(t *testing.T) {
	t.Parallel()

	djen := predicateParser{canParse: bySource(SourceDJEN), tag: "djen"}
	datajud := predicateParser{canParse: bySource(SourceDATAJUD), tag: "datajud"}
	mp := NewMultiParser(djen, datajud)

	cases := []struct {
		name    string
		payload RawPayload
		wantTag string
	}{
		{"djen payload → djen parser", RawPayload{Source: SourceDJEN}, "djen"},
		{"datajud payload → datajud parser", RawPayload{Source: SourceDATAJUD}, "datajud"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := mp.Parse(tc.payload)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if len(got.CourtRecords) != 1 || got.CourtRecords[0].Court != tc.wantTag {
				t.Fatalf("Parse() routed to %v, want tag %q", got.CourtRecords, tc.wantTag)
			}
		})
	}
}

// Priority order: when two members claim the same payload, the first listed wins.
func TestMultiParser_ParsePrefersFirstListed(t *testing.T) {
	t.Parallel()

	first := predicateParser{canParse: func(RawPayload) bool { return true }, tag: "first"}
	second := predicateParser{canParse: func(RawPayload) bool { return true }, tag: "second"}
	mp := NewMultiParser(first, second)

	got, err := mp.Parse(RawPayload{Source: SourceDJEN})
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got.CourtRecords[0].Court != "first" {
		t.Fatalf("Parse() = %q, want the first listed member", got.CourtRecords[0].Court)
	}
}

// A payload no member claims is a wiring bug: Parse returns ErrNoParserForPayload
// so the sync use case fails the run terminally.
func TestMultiParser_ParseNoMatch(t *testing.T) {
	t.Parallel()

	djen := predicateParser{canParse: bySource(SourceDJEN), tag: "djen"}
	mp := NewMultiParser(djen)

	_, err := mp.Parse(RawPayload{Source: SourceDATAJUD, ConnectorID: "stub"})
	if !errors.Is(err, ErrNoParserForPayload) {
		t.Fatalf("Parse() error = %v, want ErrNoParserForPayload", err)
	}
}

// CanParse is the OR of the members: true when any claims the payload.
func TestMultiParser_CanParse(t *testing.T) {
	t.Parallel()

	djen := predicateParser{canParse: bySource(SourceDJEN), tag: "djen"}
	mp := NewMultiParser(djen)

	if !mp.CanParse(RawPayload{Source: SourceDJEN}) {
		t.Error("CanParse(DJEN) = false, want true")
	}
	if mp.CanParse(RawPayload{Source: SourceDATAJUD}) {
		t.Error("CanParse(DATAJUD) = true, want false")
	}
}
