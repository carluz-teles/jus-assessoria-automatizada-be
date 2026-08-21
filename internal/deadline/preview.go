package deadline

import (
	"context"
	"time"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/pkg/tribunal"
)

// preview.go is the confirmation panel's live date preview (POST /v1/prazos/preview, §3): as the
// lawyer tweaks the termo inicial / dias / contagem / dobro / feriados, the FE asks the backend
// to recompute the end_date WITHOUT persisting anything. It is READ-ONLY (no UoW, off the
// transactional path) and reuses the SAME calendar engine as Confirm/Adjust (uc.computeWithExtra),
// so the preview and the eventual confirm land on the byte-for-byte same dates (paridade).

// PreviewContext is the read the preview needs when it anchors on an intimação: the three
// observed dates (to map the chosen AnchorEvent to a start) plus the process's court sigla (for
// the recompute's state-holiday UF). It is the pool-backed sibling of IntimationAnchors.
type PreviewContext struct {
	Anchors IntimationAnchors
	Court   string
}

// PreviewCommand is the preview input the handler builds from the request + the principal's
// tenant. It carries EITHER an IntimationID (the panel anchors on one of the intimação's dates,
// re-counting for the chosen AnchorEvent) OR an explicit StartDate (the manual case, no
// intimação — national holidays only, no court). TenantID comes from the verified principal.
type PreviewCommand struct {
	TenantID        string
	IntimationID    string
	StartDate       *time.Time
	AnchorEvent     AnchorEvent
	Kind            string
	Days            int
	Counting        Counting
	Doubled         bool
	ManualExtraDays int
}

// PreviewResult is the recomputed prazo dates the panel renders live — never persisted. Weekday
// is the pt-BR name of the end_date's day (e.g. "sexta"); DaysLeft is the calendar days from
// today to the end_date; HolidaysApplied is the auditable skipped-days list.
type PreviewResult struct {
	StartDate       time.Time
	EndDate         time.Time
	Weekday         string
	DaysLeft        int
	HolidaysApplied []time.Time
}

// Preview recomputes a prazo's dates for the confirmation panel WITHOUT persisting (§3). It reuses
// the same engine as Confirm/Adjust: it resolves the start (from the intimação's chosen anchor, or
// the explicit manual StartDate), then runs computeWithExtra over the SAME motor — so the preview
// matches the confirm exactly. It is read-only: it uses the injected pool (no UoW), scoped to the
// principal's tenant (barrier 1). A preview with neither an intimation nor a start is a client
// error; a missing intimação is ErrDeadlineNotFound.
func (uc *UseCase) Preview(ctx context.Context, cmd PreviewCommand) (PreviewResult, error) {
	if uc.pool == nil {
		// Defensive: the api always injects the pool via WithPreviewPool; a nil pool means a
		// misconfigured composition, not client input.
		return PreviewResult{}, apperr.NewInfra("preview pool not configured", nil)
	}

	start, uf, court, err := uc.previewStart(ctx, cmd)
	if err != nil {
		return PreviewResult{}, err
	}

	endDate, holidays, err := uc.computeWithExtra(ctx, cmd.Counting, start, cmd.Days, cmd.Doubled, cmd.ManualExtraDays, uf, court)
	if err != nil {
		return PreviewResult{}, err
	}
	if !endDate.After(start) {
		return PreviewResult{}, apperr.NewInvalid("deadline end date must be after start date")
	}

	return PreviewResult{
		StartDate:       start,
		EndDate:         endDate,
		Weekday:         weekdayPtBR(endDate),
		DaysLeft:        daysBetween(startOfDay(uc.now()), endDate),
		HolidaysApplied: holidays,
	}, nil
}

// previewStart resolves the preview's start date + the calendar's UF/court. The intimation case
// reads the anchors + court on the pool and maps the AnchorEvent; the manual case uses the
// explicit StartDate with no court (national holidays only).
func (uc *UseCase) previewStart(ctx context.Context, cmd PreviewCommand) (start time.Time, uf, court string, err error) {
	switch {
	case cmd.IntimationID != "":
		pctx, e := uc.repo.GetPreviewContext(ctx, uc.pool, cmd.IntimationID, cmd.TenantID)
		if e != nil {
			return time.Time{}, "", "", e
		}
		return pctx.Anchors.startFor(cmd.AnchorEvent), tribunal.UF(pctx.Court), pctx.Court, nil
	case cmd.StartDate != nil:
		return startOfDay(*cmd.StartDate), "", "", nil
	default:
		return time.Time{}, "", "", apperr.NewInvalid("preview requires an intimation_id or a start_date")
	}
}

// weekdayPtBR is the pt-BR name of a date's weekday (e.g. "sexta"), the panel's "vence numa
// sexta" hint. It is a small closed lookup — no i18n dependency for five labels.
func weekdayPtBR(t time.Time) string {
	switch t.Weekday() {
	case time.Sunday:
		return "domingo"
	case time.Monday:
		return "segunda"
	case time.Tuesday:
		return "terça"
	case time.Wednesday:
		return "quarta"
	case time.Thursday:
		return "quinta"
	case time.Friday:
		return "sexta"
	default:
		return "sábado"
	}
}

// daysBetween is the whole calendar days from `from` to `to` (both floored to the day). Negative
// when `to` is already past — the panel shows "vencido há N dias".
func daysBetween(from, to time.Time) int {
	return int(startOfDay(to).Sub(startOfDay(from)).Hours() / 24)
}
