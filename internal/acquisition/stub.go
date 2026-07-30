package acquisition

import (
	"context"
	"time"
)

// stub.go is the placeholder connector+parser this slice ships behind the
// connector.go ports until the REAL DJEN/DataJud connectors and parsers land
// (future sub-slices). They perform no network I/O and no format parsing: Fetch
// returns a fixed in-memory payload and Parse returns a deterministic fixture,
// so the whole sync cycle — run bookkeeping, upserts, dedup, outbox — runs end
// to end and is exercisable by the integration tests.

const (
	stubConnectorID      = "stub"
	stubConnectorVersion = "v0"
)

// StubConnector satisfies Connector with a canned payload. It is wired per source
// via the Orchestrator at composition time.
type StubConnector struct {
	source string
}

var _ Connector = (*StubConnector)(nil)

// NewStubConnector builds a stub connector tagged with the source it stands in
// for (so the sync_run and docket entries carry a realistic source).
func NewStubConnector(source string) *StubConnector {
	return &StubConnector{source: source}
}

func (c *StubConnector) ID() string      { return stubConnectorID }
func (c *StubConnector) Version() string { return stubConnectorVersion }

func (c *StubConnector) Capabilities() []Capability {
	return []Capability{CapabilityDiscoverByOAB, CapabilityFetchByNumber}
}

// Fetch returns the canned payload; it never fails (the real connector's network
// faults are a future concern).
func (c *StubConnector) Fetch(_ context.Context, _ FetchRequest) (RawPayload, error) {
	return RawPayload{
		ConnectorID:      stubConnectorID,
		ConnectorVersion: stubConnectorVersion,
		Source:           c.source,
		Body:             []byte("stub-fixture"),
	}, nil
}

// StubParser satisfies Parser: it accepts any stub payload and returns a fixed
// ParsedResult (one court record, two docket entries, one notification) so the
// upsert and dedup paths have real data to work on.
type StubParser struct{}

var _ Parser = StubParser{}

// NewStubParser builds the stub parser.
func NewStubParser() StubParser { return StubParser{} }

func (StubParser) CanParse(p RawPayload) bool { return p.ConnectorID == stubConnectorID }

func (StubParser) Parse(p RawPayload) (ParsedResult, error) {
	return stubFixture(p.Source), nil
}

// stubFixture is the deterministic parsed result: one G1 court record with two
// docket entries and one notification, all keyed to the same CNJ so the use
// case's record→entry resolution is exercised. The hashes are stable, so a
// re-sync of the same window deduplicates to zero new items.
func stubFixture(source string) ParsedResult {
	const cnj = "0000001-11.2024.8.26.0100"

	occurred := time.Date(2024, 1, 10, 12, 0, 0, 0, time.UTC)
	observed := time.Date(2024, 1, 11, 8, 0, 0, 0, time.UTC)
	madeAvailable := time.Date(2024, 1, 12, 0, 0, 0, 0, time.UTC)
	published := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	deadlineStart := time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC)

	return ParsedResult{
		CourtRecords: []ParsedCourtRecord{{
			CNJNumber:    cnj,
			Degree:       DegreeG1,
			Court:        "TJSP",
			Class:        "Procedimento Comum",
			Subject:      "Indenização",
			Completeness: 0.5,
		}},
		DocketEntries: []ParsedDocketEntry{
			{
				CNJNumber:  cnj,
				Degree:     DegreeG1,
				Hash:       "stub-docket-1",
				OccurredAt: occurred,
				ObservedAt: observed,
				Source:     source,
				Fidelity:   100,
				Text:       "Distribuído por dependência",
			},
			{
				CNJNumber:  cnj,
				Degree:     DegreeG1,
				Hash:       "stub-docket-2",
				OccurredAt: occurred.Add(24 * time.Hour),
				ObservedAt: observed.Add(24 * time.Hour),
				Source:     source,
				Fidelity:   100,
				Text:       "Concluso para despacho",
			},
		},
		Notifications: []ParsedNotification{{
			CNJNumber:       cnj,
			Degree:          DegreeG1,
			Hash:            "stub-notif-1",
			MadeAvailableAt: madeAvailable,
			PublishedAt:     published,
			DeadlineStartAt: deadlineStart,
			Content:         "Intimação para manifestação",
			Source:          source,
		}},
	}
}
