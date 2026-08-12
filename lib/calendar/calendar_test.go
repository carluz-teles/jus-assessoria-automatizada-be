package calendar

import (
	"context"
	"errors"
	"testing"
	"time"
)

// d parses a YYYY-MM-DD civil date (UTC), matching how the Calendar normalizes.
func d(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

// fakeSource is an in-memory HolidaySource: the business-day logic is verified
// without a database. Keys are YYYY-MM-DD; state/court are keyed by scope_id.
type fakeSource struct {
	national map[string]bool
	state    map[string]map[string]bool
	court    map[string]map[string]bool
}

func (f fakeSource) IsHoliday(_ context.Context, day time.Time, uf, court string) (bool, error) {
	k := day.Format(time.DateOnly)
	if f.national[k] {
		return true, nil
	}
	if uf != "" && f.state[uf][k] {
		return true, nil
	}
	if court != "" && f.court[court][k] {
		return true, nil
	}
	return false, nil
}

// errSource always fails — used to assert the Calendar propagates source errors.
type errSource struct{}

func (errSource) IsHoliday(context.Context, time.Time, string, string) (bool, error) {
	return false, errors.New("boom")
}

// allHolidaySource marks every day a holiday — used to force the prorrogação
// walk to exhaust maxScan without ever finding a business day.
type allHolidaySource struct{}

func (allHolidaySource) IsHoliday(context.Context, time.Time, string, string) (bool, error) {
	return true, nil
}

// sample fixture: Independência (07/09) and Ano Novo (01/01) national; Revolução
// Constitucionalista (09/07) as a São Paulo state holiday; a court-only holiday
// for TJSP. Dates chosen so weekday/recess do not mask the scope under test.
func sample() fakeSource {
	return fakeSource{
		national: map[string]bool{"2026-01-01": true, "2026-09-07": true},
		state:    map[string]map[string]bool{"SP": {"2026-07-09": true}},
		court:    map[string]map[string]bool{"TJSP": {"2026-05-20": true}},
	}
}

func TestCalendar_IsBusinessDay(t *testing.T) {
	t.Parallel()
	cal := New(sample())

	tests := []struct {
		name  string
		day   string
		uf    string
		court string
		want  bool
	}{
		{"feriado nacional numa segunda", "2026-09-07", "", "", false},
		{"dia util normal (terca)", "2026-02-03", "", "", true},
		{"sabado", "2026-02-07", "", "", false},
		{"domingo", "2026-02-08", "", "", false},
		{"recesso - ultimo dia 20/01", "2026-01-20", "", "", false},
		{"primeiro dia util apos recesso 21/01", "2026-01-21", "", "", true},
		{"recesso - dia util no meio 22/12", "2026-12-22", "", "", false},
		{"feriado estadual SP", "2026-07-09", "SP", "", false},
		{"mesma data em RJ nao e feriado", "2026-07-09", "RJ", "", true},
		{"feriado nacional dentro do recesso 01/01", "2026-01-01", "", "", false},
		{"feriado de tribunal TJSP", "2026-05-20", "", "TJSP", false},
		{"mesma data em outro tribunal", "2026-05-20", "", "TJRJ", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := cal.IsBusinessDay(context.Background(), d(tt.day), tt.uf, tt.court)
			if err != nil {
				t.Fatalf("IsBusinessDay(%s) unexpected error: %v", tt.day, err)
			}
			if got != tt.want {
				t.Errorf("IsBusinessDay(%s, %q, %q) = %v, want %v", tt.day, tt.uf, tt.court, got, tt.want)
			}
		})
	}
}

func TestCalendar_IsBusinessDay_SourceError(t *testing.T) {
	t.Parallel()
	cal := New(errSource{})
	// A plain weekday, outside the recess, so the weekend/recess short-circuit
	// does not hide the source call.
	if _, err := cal.IsBusinessDay(context.Background(), d("2026-02-03"), "", ""); err == nil {
		t.Fatal("expected error from source, got nil")
	}
}

func TestCalendar_NextBusinessDay(t *testing.T) {
	t.Parallel()
	cal := New(sample())

	tests := []struct {
		name string
		from string
		want string
	}{
		{"sexta pula fim de semana e feriado de segunda", "2026-09-04", "2026-09-08"},
		{"terca comum avanca um dia", "2026-02-03", "2026-02-04"},
		{"vespera do recesso salta pro pos-recesso", "2026-12-18", "2027-01-21"},
		{"dentro do recesso salta pro pos-recesso", "2026-12-31", "2027-01-21"},
		{"ultimo dia util antes do fim do recesso", "2026-01-19", "2026-01-21"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := cal.NextBusinessDay(context.Background(), d(tt.from), "", "")
			if err != nil {
				t.Fatalf("NextBusinessDay(%s) unexpected error: %v", tt.from, err)
			}
			if !got.Equal(d(tt.want)) {
				t.Errorf("NextBusinessDay(%s) = %s, want %s", tt.from, got.Format(time.DateOnly), tt.want)
			}
		})
	}
}

func TestCalendar_AddBusinessDays(t *testing.T) {
	t.Parallel()
	cal := New(sample())

	tests := []struct {
		name        string
		start       string
		n           int
		wantEnd     string
		mustContain []string // dates that MUST appear in the skipped/audit list
		wantEmpty   bool     // skipped must be empty
	}{
		{name: "n=0 devolve o proprio start", start: "2026-02-03", n: 0, wantEnd: "2026-02-03", wantEmpty: true},
		{name: "3 dias uteis, exclui o start (CPC 224)", start: "2026-02-03", n: 3, wantEnd: "2026-02-06", wantEmpty: true},
		{name: "pula feriado de segunda e o audita", start: "2026-09-04", n: 2, wantEnd: "2026-09-09", mustContain: []string{"2026-09-07"}},
		{name: "atravessa o recesso e audita o natal", start: "2026-12-17", n: 2, wantEnd: "2027-01-21", mustContain: []string{"2026-12-25"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			end, skipped, err := cal.AddBusinessDays(context.Background(), d(tt.start), tt.n, "", "")
			if err != nil {
				t.Fatalf("AddBusinessDays(%s, %d) unexpected error: %v", tt.start, tt.n, err)
			}
			if !end.Equal(d(tt.wantEnd)) {
				t.Errorf("end = %s, want %s", end.Format(time.DateOnly), tt.wantEnd)
			}
			if tt.wantEmpty && len(skipped) != 0 {
				t.Errorf("skipped = %v, want empty", fmtDates(skipped))
			}
			for _, want := range tt.mustContain {
				if !containsDate(skipped, d(want)) {
					t.Errorf("skipped %v missing %s", fmtDates(skipped), want)
				}
			}
			// Sanity: no weekend day is ever audited (only holidays/recess).
			for _, s := range skipped {
				if isWeekend(s) {
					t.Errorf("skipped contains weekend %s", s.Format(time.DateOnly))
				}
			}
		})
	}
}

func TestCalendar_AddBusinessDays_NegativeN(t *testing.T) {
	t.Parallel()
	cal := New(sample())
	if _, _, err := cal.AddBusinessDays(context.Background(), d("2026-02-03"), -1, "", ""); err == nil {
		t.Fatal("expected error for negative n, got nil")
	}
}

func TestCalendar_AddBusinessDays_SourceError(t *testing.T) {
	t.Parallel()
	cal := New(errSource{})
	if _, _, err := cal.AddBusinessDays(context.Background(), d("2026-02-03"), 1, "", ""); err == nil {
		t.Fatal("expected error from source, got nil")
	}
}

func TestCalendar_AddCalendarDays(t *testing.T) {
	t.Parallel()
	cal := New(sample())

	tests := []struct {
		name        string
		start       string
		n           int
		wantEnd     string
		mustContain []string // dates that MUST appear in the prorrogação audit list
		wantEmpty   bool     // audit list must be empty
	}{
		// U-cal-1: dia de início excluído; fim de semana conta no cômputo e o
		// vencimento cai num dia útil, sem prorrogação → nada auditado.
		{name: "n=0 devolve o proprio start normalizado", start: "2026-02-03", n: 0, wantEnd: "2026-02-03", wantEmpty: true},
		{name: "10 corridos, start numa segunda, vence em dia util", start: "2026-02-02", n: 10, wantEnd: "2026-02-12", wantEmpty: true},
		// U-cal-2a: vencimento cai no sábado → prorroga pra segunda útil; só
		// pulou fim de semana, que NÃO é auditado (espelha AddBusinessDays).
		{name: "vencimento no sabado prorroga pra segunda, fds nao auditado", start: "2026-02-02", n: 5, wantEnd: "2026-02-09", wantEmpty: true},
		// U-cal-3: o n-ésimo dia corrido (26/12) cai DENTRO do recesso →
		// prorroga pro 1º dia útil após 20/01; os dias úteis de recesso pulados
		// no fim são auditados (28/12 é uma segunda de recesso).
		{name: "vencimento dentro do recesso prorroga pro pos-recesso", start: "2026-12-15", n: 11, wantEnd: "2027-01-21", mustContain: []string{"2026-12-28"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			end, applied, err := cal.AddCalendarDays(context.Background(), d(tt.start), tt.n, "", "")
			if err != nil {
				t.Fatalf("AddCalendarDays(%s, %d) unexpected error: %v", tt.start, tt.n, err)
			}
			if !end.Equal(d(tt.wantEnd)) {
				t.Errorf("end = %s, want %s", end.Format(time.DateOnly), tt.wantEnd)
			}
			if tt.wantEmpty && len(applied) != 0 {
				t.Errorf("holidays_applied = %v, want empty", fmtDates(applied))
			}
			for _, want := range tt.mustContain {
				if !containsDate(applied, d(want)) {
					t.Errorf("holidays_applied %v missing %s", fmtDates(applied), want)
				}
			}
			// Fins de semana nunca são auditados — só feriado/recesso da prorrogação.
			for _, s := range applied {
				if isWeekend(s) {
					t.Errorf("holidays_applied contains weekend %s", s.Format(time.DateOnly))
				}
			}
		})
	}
}

// U-cal-2b: quando a prorrogação atravessa um FERIADO (e não só fim de semana),
// o feriado É auditado, mas o sábado/domingo pulados no caminho NÃO. Vencimento
// cai no sábado 07/02, a segunda 09/02 é feriado → prorroga pra terça 10/02.
func TestCalendar_AddCalendarDays_ProrogaFeriado(t *testing.T) {
	t.Parallel()
	src := sample()
	src.national["2026-02-09"] = true
	cal := New(src)

	end, applied, err := cal.AddCalendarDays(context.Background(), d("2026-02-02"), 5, "", "")
	if err != nil {
		t.Fatalf("AddCalendarDays unexpected error: %v", err)
	}
	if !end.Equal(d("2026-02-10")) {
		t.Errorf("end = %s, want 2026-02-10", end.Format(time.DateOnly))
	}
	if !containsDate(applied, d("2026-02-09")) {
		t.Errorf("holidays_applied %v missing feriado 2026-02-09", fmtDates(applied))
	}
	for _, weekend := range []string{"2026-02-07", "2026-02-08"} {
		if containsDate(applied, d(weekend)) {
			t.Errorf("holidays_applied %v must not audit weekend %s", fmtDates(applied), weekend)
		}
	}
}

func TestCalendar_AddCalendarDays_NegativeN(t *testing.T) {
	t.Parallel()
	cal := New(sample())
	if _, _, err := cal.AddCalendarDays(context.Background(), d("2026-02-03"), -1, "", ""); err == nil {
		t.Fatal("expected error for negative n, got nil")
	}
}

func TestCalendar_AddCalendarDays_SourceError(t *testing.T) {
	t.Parallel()
	cal := New(errSource{})
	if _, _, err := cal.AddCalendarDays(context.Background(), d("2026-02-03"), 1, "", ""); err == nil {
		t.Fatal("expected error from source, got nil")
	}
}

// U-cal-4: um vencimento que só encontra dias não-úteis à frente estoura o
// maxScan da prorrogação em vez de varrer infinito.
func TestCalendar_AddCalendarDays_MaxScan(t *testing.T) {
	t.Parallel()
	cal := New(allHolidaySource{})
	if _, _, err := cal.AddCalendarDays(context.Background(), d("2026-02-02"), 1, "", ""); err == nil {
		t.Fatal("expected maxScan error, got nil")
	}
}

func containsDate(days []time.Time, target time.Time) bool {
	for _, x := range days {
		if x.Equal(target) {
			return true
		}
	}
	return false
}

func fmtDates(days []time.Time) []string {
	out := make([]string, len(days))
	for i, x := range days {
		out[i] = x.Format(time.DateOnly)
	}
	return out
}
