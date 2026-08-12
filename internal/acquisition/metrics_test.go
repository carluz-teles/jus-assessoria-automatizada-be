package acquisition

import (
	"strconv"
	"testing"
)

// httpStatusClass buckets DJEN status codes for the request counter; 429 gets its own
// class (a rate block, operationally distinct from a plain 4xx), 3xx/0 fall to "other".
func TestHTTPStatusClass(t *testing.T) {
	tests := []struct {
		code int
		want string
	}{
		{200, "2xx"},
		{201, "2xx"},
		{299, "2xx"},
		{429, "429"},
		{400, "4xx"},
		{404, "4xx"},
		{499, "4xx"},
		{500, "5xx"},
		{503, "5xx"},
		{301, "other"},
		{100, "other"},
		{0, "other"},
	}

	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.code), func(t *testing.T) {
			t.Parallel()
			if got := httpStatusClass(tt.code); got != tt.want {
				t.Errorf("httpStatusClass(%d) = %q, want %q", tt.code, got, tt.want)
			}
		})
	}
}
