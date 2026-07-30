package acquisition

import (
	"errors"
	"testing"
)

// A registered source resolves to its connector; an unregistered one is the
// typed ErrConnectorNotFound.
func TestOrchestrator_ConnectorFor(t *testing.T) {
	t.Parallel()

	orch := NewOrchestrator()
	djen := NewStubConnector(SourceDJEN)
	orch.Register(SourceDJEN, djen)

	got, err := orch.ConnectorFor(SourceDJEN)
	if err != nil {
		t.Fatalf("ConnectorFor(DJEN) error = %v", err)
	}
	if got != djen {
		t.Fatalf("ConnectorFor(DJEN) = %v, want the registered connector", got)
	}

	if _, err := orch.ConnectorFor(SourceDATAJUD); !errors.Is(err, ErrConnectorNotFound) {
		t.Fatalf("ConnectorFor(unregistered) error = %v, want ErrConnectorNotFound", err)
	}
}

// Registering the same source twice keeps the last connector.
func TestOrchestrator_Register_LastWins(t *testing.T) {
	t.Parallel()

	orch := NewOrchestrator()
	first := NewStubConnector(SourceDJEN)
	second := NewStubConnector(SourceDJEN)
	orch.Register(SourceDJEN, first)
	orch.Register(SourceDJEN, second)

	got, err := orch.ConnectorFor(SourceDJEN)
	if err != nil {
		t.Fatalf("ConnectorFor(DJEN) error = %v", err)
	}
	if got != second {
		t.Fatalf("ConnectorFor(DJEN) = %v, want the last-registered connector", got)
	}
}
