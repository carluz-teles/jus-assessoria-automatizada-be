package deadline

import (
	"testing"
	"time"
)

// --- deriveDisplayStatus (the single presentation-status derivation) ---------

// TestDeriveDisplayStatus_FourCases is the core of requirement (2): the derived display_status
// must be correct across the four buckets (Concluída, Atrasada, Em execução, Aberta) plus the
// DISMISSED "no bucket" case, from (status, checklist progress, due_date) against a fixed today.
func TestDeriveDisplayStatus_FourCases(t *testing.T) {
	t.Parallel()

	today := time.Date(2024, 3, 20, 12, 0, 0, 0, time.UTC)
	yesterday := today.AddDate(0, 0, -1)
	tomorrow := today.AddDate(0, 0, 1)

	tests := []struct {
		name     string
		status   TaskStatus
		progress TaskProgress
		dueDate  *time.Time
		want     DisplayStatus
	}{
		{
			name:   "DONE is Concluída regardless of progress/due",
			status: TaskStatusDone, progress: TaskProgress{Done: 0, Total: 3}, dueDate: &yesterday,
			want: DisplayConcluida,
		},
		{
			name:   "OPEN overdue is Atrasada (beats in-progress)",
			status: TaskStatusOpen, progress: TaskProgress{Done: 2, Total: 3}, dueDate: &yesterday,
			want: DisplayAtrasada,
		},
		{
			name:   "OPEN with some item done and not overdue is Em execução",
			status: TaskStatusOpen, progress: TaskProgress{Done: 1, Total: 3}, dueDate: &tomorrow,
			want: DisplayEmExecucao,
		},
		{
			name:   "OPEN with no item done and not overdue is Aberta",
			status: TaskStatusOpen, progress: TaskProgress{Done: 0, Total: 3}, dueDate: &tomorrow,
			want: DisplayAberta,
		},
		{
			name:   "OPEN undated with no progress is Aberta (no due_date is never overdue)",
			status: TaskStatusOpen, progress: TaskProgress{}, dueDate: nil,
			want: DisplayAberta,
		},
		{
			name:   "OPEN due today is NOT overdue (Atrasada is strictly before today)",
			status: TaskStatusOpen, progress: TaskProgress{}, dueDate: &today,
			want: DisplayAberta,
		},
		{
			name:   "DISMISSED has no bucket",
			status: TaskStatusDismissed, progress: TaskProgress{Done: 1, Total: 1}, dueDate: &yesterday,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := deriveDisplayStatus(tt.status, tt.progress, tt.dueDate, today)
			if got != tt.want {
				t.Errorf("deriveDisplayStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}
