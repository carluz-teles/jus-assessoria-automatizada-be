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
