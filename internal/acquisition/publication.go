package acquisition

import (
	"encoding/json"
	"time"
)

// PublicationParams is one national DJEN communication to land in the publication
// firehose. Court is the siglaTribunal (see the tribunal registry); MadeAvailableAt
// is the disponibilização date; RecipientOABs are the normalized "NUMBER|UF" keys the
// local OAB match indexes; Payload is the raw DJEN item the parser later consumes for
// a matched tenant. It is national reference data — no tenant scope.
type PublicationParams struct {
	Hash            string
	Court           string
	CNJNumber       string
	MadeAvailableAt time.Time
	RecipientOABs   []string
	Payload         json.RawMessage
}

// PublicationMatch is one hit of the national match: a tenant watches OABKey, which is
// a recipient of the publication whose raw item is Payload. The match use case groups
// these by tenant and re-parses the payloads to create that tenant's intimações.
type PublicationMatch struct {
	TenantID string
	OABKey   string
	Payload  json.RawMessage
}
