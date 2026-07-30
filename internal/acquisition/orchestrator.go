package acquisition

// orchestrator.go is the minimal connector registry: given a data source it
// returns the connector that serves it. It is what the sync composition uses to
// pick a connector to drive; there is no circuit breaker, failover, or health
// gating in this slice (those are later concerns).

// Orchestrator maps a source (DJEN, DATAJUD, …) to its Connector.
type Orchestrator struct {
	bySource map[string]Connector
}

// NewOrchestrator returns an empty registry; callers Register the connectors
// they wire (a stub in this slice).
func NewOrchestrator() *Orchestrator {
	return &Orchestrator{bySource: map[string]Connector{}}
}

// Register binds a connector to the source it serves. Registering the same
// source twice keeps the last connector (last wins).
func (o *Orchestrator) Register(source string, c Connector) {
	o.bySource[source] = c
}

// ConnectorFor returns the connector registered for source, or the typed
// ErrConnectorNotFound when none is.
func (o *Orchestrator) ConnectorFor(source string) (Connector, error) {
	c, ok := o.bySource[source]
	if !ok {
		return nil, ErrConnectorNotFound
	}
	return c, nil
}
