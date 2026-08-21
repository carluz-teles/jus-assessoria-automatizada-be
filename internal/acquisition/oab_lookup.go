package acquisition

import (
	"context"
	"encoding/json"
	"time"
)

// oab_lookup.go is a best-effort substitute for a dedicated OAB registry API —
// none exists free and public (the OAB's own CNA web service requires an
// institutional Authentication Key; third-party providers are paid). It reuses
// the DJEN connector instead: the SAME public consulta that discovers a
// tenant's processes by OAB also names that OAB's holder in every
// communication's destinatarioadvogados. One page, on demand — not the
// multi-page backfill walk — because this backs a live UI call (onboarding,
// Termos), not an async sweep.

// LawyerLookup is the port to resolve an OAB registration's holder name from a
// recent public communication. NotFound (ErrOABNotFound) means no recent DJEN
// communication named this OAB — NOT that the OAB is malformed or invalid; the
// caller degrades gracefully (a placeholder name), it never blocks on this.
type LawyerLookup interface {
	LookupOABName(ctx context.Context, oab OABEntry) (string, error)
}

var _ LawyerLookup = (*DJENConnector)(nil)

// oabLookupWindowDays bounds how far back the on-demand search looks — one
// year, the same horizon as the onboarding backfill: an OAB active enough to
// be worth auto-naming has very likely published something in that window, and
// DJEN returns items ordered so a single page (up to c.pageSize, default 1000)
// reliably catches it without a multi-page walk.
const oabLookupWindowDays = 365

// LookupOABName performs ONE consulta GET (via the shared getPage — same
// HTTP/WAF/rate-limit machinery the backfill path uses) and returns the name of
// the first destinatarioadvogados entry whose OAB matches. ErrOABNotFound when
// no item in the window names it. Any transport/rate-limit fault surfaces as
// the same typed, retryable errors getPage already produces.
func (c *DJENConnector) LookupOABName(ctx context.Context, oab OABEntry) (string, error) {
	to := time.Now().UTC().Format(djenDateLayout)
	from := time.Now().UTC().AddDate(0, 0, -oabLookupWindowDays).Format(djenDateLayout)

	resp, err := c.getPage(ctx, oab, from, to, 1)
	if err != nil {
		return "", err
	}

	target := oabKey(oab.Number, oab.UF)
	for _, raw := range resp.Items {
		var item djenComunicacao
		if err := json.Unmarshal(raw, &item); err != nil {
			continue
		}
		for _, link := range item.Advogados {
			if oabKey(link.Advogado.NumeroOAB, link.Advogado.UFOAB) == target {
				return link.Advogado.Nome, nil
			}
		}
	}
	return "", ErrOABNotFound
}
