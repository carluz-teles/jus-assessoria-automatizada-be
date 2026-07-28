package storage_test

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/storage"
)

// fullOptions is a complete, valid config. Endpoint is set so tests exercise the
// S3-compatible (R2/MinIO) path; presigning is entirely local, no network.
func fullOptions() storage.Options {
	return storage.Options{
		Endpoint:     "https://accountid.r2.cloudflarestorage.com",
		Region:       "auto",
		Bucket:       "minutas",
		AccessKey:    "AKIAEXAMPLE",
		SecretKey:    "secretexamplekey",
		UsePathStyle: true,
	}
}

func TestNew(t *testing.T) {
	t.Parallel()

	t.Run("full options build a client", func(t *testing.T) {
		t.Parallel()

		client, err := storage.New(context.Background(), fullOptions())
		if err != nil {
			t.Fatalf("New() error = %v, want nil", err)
		}
		if client == nil {
			t.Fatal("New() returned nil client")
		}
	})

	missing := []struct {
		name   string
		mutate func(*storage.Options)
	}{
		{"missing bucket", func(o *storage.Options) { o.Bucket = "" }},
		{"missing region", func(o *storage.Options) { o.Region = "" }},
		{"missing access key", func(o *storage.Options) { o.AccessKey = "" }},
		{"missing secret key", func(o *storage.Options) { o.SecretKey = "" }},
	}
	for _, tt := range missing {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := fullOptions()
			tt.mutate(&opts)

			client, err := storage.New(context.Background(), opts)
			if client != nil {
				t.Errorf("New() client = %v, want nil on invalid config", client)
			}

			var appErr *apperr.AppError
			if !errors.As(err, &appErr) {
				t.Fatalf("New() error = %v, want *apperr.AppError", err)
			}
			if appErr.Kind != apperr.KindInvalid {
				t.Errorf("New() error kind = %q, want %q", appErr.Kind, apperr.KindInvalid)
			}
		})
	}
}

func TestClient_PresignedPut(t *testing.T) {
	t.Parallel()

	client := newTestClient(t)

	got, err := client.PresignedPut(context.Background(), "minutas/drafts/abc", "application/pdf", 5*time.Minute)
	if err != nil {
		t.Fatalf("PresignedPut() error = %v, want nil", err)
	}

	assertPresignedURL(t, got, "minutas", "minutas/drafts/abc")
}

func TestClient_PresignedGet(t *testing.T) {
	t.Parallel()

	client := newTestClient(t)

	got, err := client.PresignedGet(context.Background(), "minutas/drafts/abc", 5*time.Minute)
	if err != nil {
		t.Fatalf("PresignedGet() error = %v, want nil", err)
	}

	assertPresignedURL(t, got, "minutas", "minutas/drafts/abc")
}

func TestNewKey(t *testing.T) {
	t.Parallel()

	keyRe := regexp.MustCompile(`^tenant/drafts/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

	first := storage.NewKey("tenant", "drafts")
	if !keyRe.MatchString(first) {
		t.Errorf("NewKey() = %q, want match %s", first, keyRe)
	}

	second := storage.NewKey("tenant", "drafts")
	if first == second {
		t.Errorf("NewKey() returned identical keys %q on two calls, want distinct", first)
	}
}

func newTestClient(t *testing.T) *storage.Client {
	t.Helper()

	client, err := storage.New(context.Background(), fullOptions())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	return client
}

// assertPresignedURL checks the URL is a valid https URL carrying the bucket,
// the key, and the SigV4 signature query parameters.
func assertPresignedURL(t *testing.T, raw, bucket, key string) {
	t.Helper()

	if raw == "" {
		t.Fatal("presigned URL is empty")
	}

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("presigned URL %q is not parseable: %v", raw, err)
	}
	if u.Scheme != "https" {
		t.Errorf("presigned URL scheme = %q, want https", u.Scheme)
	}
	if !strings.Contains(raw, bucket) {
		t.Errorf("presigned URL %q does not contain bucket %q", raw, bucket)
	}
	if !strings.Contains(u.Path, key) {
		t.Errorf("presigned URL path %q does not contain key %q", u.Path, key)
	}

	q := u.Query()
	for _, param := range []string{"X-Amz-Algorithm", "X-Amz-Credential", "X-Amz-Signature"} {
		if q.Get(param) == "" {
			t.Errorf("presigned URL missing signature param %q; query = %v", param, q)
		}
	}
}
