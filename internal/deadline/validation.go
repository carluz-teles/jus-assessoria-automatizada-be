package deadline

import (
	"fmt"

	"github.com/jusassessoria/platform/lib/apperr"
)

// validate is the invariant check on a derived prazo BEFORE it is persisted — a
// belt-and-suspenders on top of the DB CHECKs (0024) for this safety-critical
// (deadline) data. The days come from the rules layer (CHECK days > 0) and the dates
// from lib/calendar, so a violation here means a bug upstream, not bad user input; it
// returns a typed KindInvalid so the fault is loud, not a silently persisted bad prazo.
func (d *Deadline) validate() error {
	if d.Days <= 0 {
		return apperr.NewInvalid(fmt.Sprintf("deadline days must be > 0, got %d", d.Days))
	}
	if d.Counting != CountingBusiness && d.Counting != CountingCalendar {
		return apperr.NewInvalid(fmt.Sprintf("invalid deadline counting %q", d.Counting))
	}
	// The end is the n-th day AFTER the start (start excluded, CPC art. 224), so it is
	// always strictly after the start; an end on/before the start is impossible math.
	if !d.EndDate.After(d.StartDate) {
		return apperr.NewInvalid("deadline end date must be after start date")
	}
	return nil
}
