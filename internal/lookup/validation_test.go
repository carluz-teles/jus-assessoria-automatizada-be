package lookup

import (
	"testing"

	"github.com/jusassessoria/platform/lib/apperr"
)

func TestNormalizeCNPJ(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "bare 14 digits", raw: "19131243000197", want: "19131243000197"},
		{name: "masked strips to digits", raw: "19.131.243/0001-97", want: "19131243000197"},
		{name: "too short passes through (no format check)", raw: "1913124300019", want: "1913124300019"},
		{name: "too long passes through (no format check)", raw: "191312430001970", want: "191312430001970"},
		{name: "letters strip out, no format check", raw: "1913124300019X", want: "1913124300019"},
		{name: "empty stays empty (no format check)", raw: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := normalizeCNPJ(tt.raw)
			assertNormalize(t, got, err, tt.want, tt.wantErr)
		})
	}
}

func TestNormalizeCEP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "bare 8 digits", raw: "01311902", want: "01311902"},
		{name: "masked strips to digits", raw: "01311-902", want: "01311902"},
		{name: "too short is invalid", raw: "0131190", wantErr: true},
		{name: "too long is invalid", raw: "013119021", wantErr: true},
		{name: "letters are invalid", raw: "0131190X", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := normalizeCEP(tt.raw)
			assertNormalize(t, got, err, tt.want, tt.wantErr)
		})
	}
}

// assertNormalize checks a normalize* result: on the error path the kind must be
// Invalid (→ 400 at the edge); on the happy path the normalized digits must match.
func assertNormalize(t *testing.T, got string, err error, want string, wantErr bool) {
	t.Helper()

	if wantErr {
		if err == nil {
			t.Fatalf("got nil error, want Invalid for %q", got)
		}
		if ae, ok := apperr.From(err); !ok || ae.Kind != apperr.KindInvalid {
			t.Fatalf("error kind = %v, want %q", err, apperr.KindInvalid)
		}
		return
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Fatalf("normalized = %q, want %q", got, want)
	}
}
