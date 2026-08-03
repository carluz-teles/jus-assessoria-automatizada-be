package calendar

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBrasilAPIFetcher_NationalHolidays(t *testing.T) {
	t.Parallel()

	const body = `[
		{"date":"2026-01-01","name":"Confraternização mundial","type":"national"},
		{"date":"2026-04-21","name":"Tiradentes","type":"national"},
		{"date":"2026-09-07","name":"Independência do Brasil","type":"national"}
	]`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/feriados/v1/2026" {
			t.Errorf("path = %q, want /feriados/v1/2026", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	fetcher := NewBrasilAPIFetcher(WithFetcherBaseURL(srv.URL))
	got, err := fetcher.NationalHolidays(context.Background(), 2026)
	if err != nil {
		t.Fatalf("NationalHolidays: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	first := got[0]
	if first.Scope != ScopeNational {
		t.Errorf("scope = %q, want %q", first.Scope, ScopeNational)
	}
	if first.ScopeID != "" {
		t.Errorf("national holiday must have empty scope_id, got %q", first.ScopeID)
	}
	if !first.Date.Equal(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("date = %s, want 2026-01-01", first.Date.Format(time.DateOnly))
	}
	if first.Name != "Confraternização mundial" {
		t.Errorf("name = %q", first.Name)
	}
}

func TestBrasilAPIFetcher_NationalHolidays_Empty(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	got, err := NewBrasilAPIFetcher(WithFetcherBaseURL(srv.URL)).NationalHolidays(context.Background(), 2099)
	if err != nil {
		t.Fatalf("empty year should not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d, want 0", len(got))
	}
}

func TestBrasilAPIFetcher_NationalHolidays_NonOK(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := NewBrasilAPIFetcher(WithFetcherBaseURL(srv.URL)).NationalHolidays(context.Background(), 2026); err == nil {
		t.Fatal("expected error on non-200, got nil")
	}
}

func TestBrasilAPIFetcher_NationalHolidays_BadDate(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[{"date":"07/09/2026","name":"data em formato errado"}]`))
	}))
	defer srv.Close()

	if _, err := NewBrasilAPIFetcher(WithFetcherBaseURL(srv.URL)).NationalHolidays(context.Background(), 2026); err == nil {
		t.Fatal("expected error on unparseable date, got nil")
	}
}
