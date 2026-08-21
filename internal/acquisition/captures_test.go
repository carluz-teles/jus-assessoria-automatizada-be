package acquisition

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// strPtr / timePtr build the nullable read-model fields the capture rows carry.
func strPtr(s string) *string    { return &s }
func timePtr(t time.Time) *time.Time { return &t }

// TestCaptureDisplayStatus maps the raw capture status + error count to the user-facing
// label the FE renders as-is (no client-side status logic).
func TestCaptureDisplayStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status string
		errs   int
		want   string
	}{
		{name: "OK no errors is done", status: "OK", errs: 0, want: captureDisplayDone},
		{name: "OK with errors is done-with-warnings", status: "OK", errs: 2, want: captureDisplayWithWarn},
		{name: "COMPLETED (initial load) no errors is done", status: "COMPLETED", errs: 0, want: captureDisplayDone},
		{name: "COMPLETED with errors is done-with-warnings", status: "COMPLETED", errs: 1, want: captureDisplayWithWarn},
		{name: "PARTIAL is partial failure", status: "PARTIAL", errs: 0, want: captureDisplayPartial},
		{name: "FAILED is partial failure", status: "FAILED", errs: 0, want: captureDisplayPartial},
		{name: "RUNNING is in progress", status: "RUNNING", errs: 0, want: captureDisplayRunning},
		{name: "unknown falls back to in progress", status: "WEIRD", errs: 0, want: captureDisplayRunning},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, captureDisplayStatus(tt.status, tt.errs))
		})
	}
}

// TestReadUseCase_Captures assembles the screen: the KPI header (with today's derived
// deadlines and the configured next execution) and one derived row per capture. A
// finished capture gets its per-window prazos/tarefas counts + duration; the OAB count
// is read once and stamped on every row.
func TestReadUseCase_Captures(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 8, 18, 6, 0, 0, 0, time.UTC)
	finish := start.Add(90 * time.Second)
	repo := &recordingReadRepo{
		captureSummary: CaptureSummaryRow{
			LastCaptureAt:       timePtr(finish),
			IntimationsNewToday: 12,
		},
		deadlinesToday: 9,
		oabCount:       3,
		deadlinesBetween: 5,
		tasksBetween:     2,
		captureRows: []CaptureRunRow{
			{
				ID: "cap-1", Source: "DJEN", Kind: "DAILY_CAPTURE",
				WindowFrom: strPtr("2026-08-18"), WindowTo: strPtr("2026-08-18"),
				StartedAt: start, FinishedAt: timePtr(finish), Status: "OK",
				CourtRecordsNew: 3, IntimationsNew: 12, CourtRecordsUpdated: 4, Errors: 0,
			},
			{
				ID: "cap-running", Source: "DJEN", Kind: "DAILY_CAPTURE",
				StartedAt: start, FinishedAt: nil, Status: "RUNNING",
			},
		},
	}
	uc := NewReadUseCase(repo, WithCaptureDailyTime("06:00"))

	view, err := uc.Captures(context.Background(), "t-1")
	require.NoError(t, err)

	// KPI header.
	assert.Equal(t, 12, view.Summary.IntimationsNewToday)
	assert.Equal(t, 9, view.Summary.DeadlinesDerivedToday)
	require.NotNil(t, view.Summary.LastCaptureAt)
	assert.Equal(t, finish, *view.Summary.LastCaptureAt)
	require.NotNil(t, view.Summary.NextExecution)
	assert.Equal(t, "06:00", *view.Summary.NextExecution)
	assert.Equal(t, "t-1", repo.gotCapturesTID)

	require.Len(t, view.Runs, 2)

	// Finished capture: display status, duration, per-window counts, OAB count.
	done := view.Runs[0]
	assert.Equal(t, "cap-1", done.ID)
	assert.Equal(t, captureDisplayDone, done.DisplayStatus)
	assert.Equal(t, 3, done.CourtRecordsNew)
	assert.Equal(t, 4, done.CourtRecordsUpdated)
	assert.Equal(t, 5, done.DeadlinesCreated)
	assert.Equal(t, 2, done.TasksCreated)
	assert.Equal(t, 3, done.OABCount)
	require.NotNil(t, done.DurationSec)
	assert.Equal(t, 90, *done.DurationSec)

	// Running capture: no duration, and per-window counts are not derived (still zero).
	running := view.Runs[1]
	assert.Equal(t, captureDisplayRunning, running.DisplayStatus)
	assert.Nil(t, running.DurationSec)
	assert.Zero(t, running.DeadlinesCreated)
	assert.Zero(t, running.TasksCreated)
	assert.Equal(t, 3, running.OABCount)
}

// TestReadUseCase_Captures_NoDailyTime keeps the next-execution honest: with
// CAPTURE_DAILY_TIME unset, the KPI header's NextExecution is nil (the FE renders "—"),
// never a fabricated wall-clock time.
func TestReadUseCase_Captures_NoDailyTime(t *testing.T) {
	t.Parallel()

	uc := NewReadUseCase(&recordingReadRepo{}, WithCaptureDailyTime(""))
	view, err := uc.Captures(context.Background(), "t-1")
	require.NoError(t, err)
	assert.Nil(t, view.Summary.NextExecution)
}

// TestReadUseCase_CaptureDetail forwards (tenant, id) to the repo and folds the row
// into the view with the derived status/duration/counts.
func TestReadUseCase_CaptureDetail(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 8, 18, 6, 0, 0, 0, time.UTC)
	finish := start.Add(30 * time.Second)
	repo := &recordingReadRepo{
		oabCount:         3,
		deadlinesBetween: 1,
		tasksBetween:     0,
		captureOne: CaptureRunRow{
			ID: "cap-1", Source: "DATAJUD", Kind: "ENRICHMENT",
			StartedAt: start, FinishedAt: timePtr(finish), Status: "OK",
			CourtRecordsUpdated: 7, Errors: 0,
		},
	}
	uc := NewReadUseCase(repo)

	view, err := uc.CaptureDetail(context.Background(), "t-1", "cap-1")
	require.NoError(t, err)

	assert.Equal(t, "t-1", repo.gotCaptureTID)
	assert.Equal(t, "cap-1", repo.gotCaptureID)
	assert.Equal(t, "ENRICHMENT", view.Kind)
	assert.Equal(t, captureDisplayDone, view.DisplayStatus)
	assert.Equal(t, 7, view.CourtRecordsUpdated)
	// ENRICHMENT só ATUALIZA processos — prazos/tarefas vêm da descoberta (carga inicial /
	// captura diária), não do enriquecimento; por isso a linha ENRICHMENT não os atribui
	// (0), mesmo com deadlinesBetween=1 no repo. Duração/status seguem derivando (terminal).
	assert.Equal(t, 0, view.DeadlinesCreated)
	assert.Equal(t, 0, view.TasksCreated)
	assert.Equal(t, 3, view.OABCount)
	require.NotNil(t, view.DurationSec)
	assert.Equal(t, 30, *view.DurationSec)
}

// TestReadUseCase_CaptureDetail_RunningHidesConclusion locks the fix for the drawer bug: a
// RUNNING capture keeps a moving finished_at (last-activity marker), but must NOT present a
// finished_at, a duration or finalized counts — otherwise the drawer shows "Concluída em:
// now()" while the header still says "Em andamento".
func TestReadUseCase_CaptureDetail_RunningHidesConclusion(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 8, 20, 9, 37, 0, 0, time.UTC)
	moving := start.Add(3 * time.Minute)
	repo := &recordingReadRepo{
		oabCount:         1,
		deadlinesBetween: 12436,
		tasksBetween:     5,
		captureOne: CaptureRunRow{
			ID: "cap-run", Source: "DATAJUD", Kind: "ENRICHMENT",
			StartedAt: start, FinishedAt: timePtr(moving), Status: SyncStatusRunning,
			CourtRecordsUpdated: 296, Errors: 0,
		},
	}
	uc := NewReadUseCase(repo)

	view, err := uc.CaptureDetail(context.Background(), "t-1", "cap-run")
	require.NoError(t, err)

	assert.Equal(t, captureDisplayRunning, view.DisplayStatus)
	assert.Equal(t, 296, view.CourtRecordsUpdated) // o contador ao vivo ainda aparece
	assert.Nil(t, view.FinishedAt)                 // não concluiu → sem "Concluída em"
	assert.Nil(t, view.DurationSec)                // sem duração falsa
	assert.Equal(t, 0, view.DeadlinesCreated)      // sem contagem final
	assert.Equal(t, 0, view.TasksCreated)
}
