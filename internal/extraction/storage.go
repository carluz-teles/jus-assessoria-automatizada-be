package extraction

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"time"

	"github.com/jusassessoria/platform/lib/apperr"
)

// storage.go is the objectStore adapter over lib/storage's presigned surface. lib/storage
// hands out presigned GET/PUT URLs (files never transit the app on the request path); this
// slice runs SERVER-SIDE in the worker, so reading/writing the bytes directly through the
// presigned URL is fine — the app is the trusted holder of the key here, not a browser.
// It presigns then does the HTTP round-trip, mapping any transport fault to a typed infra
// error (retryable at the listener).

// presignedTTL is how long the GET/PUT URLs this adapter mints stay valid. A minute is
// ample for a single server-side round-trip and short enough that a leaked URL is useless.
const presignedTTL = time.Minute

// presigner is the slice of lib/storage.Client the adapter needs. Depending on it (not the
// concrete *storage.Client) keeps the adapter testable and the coupling narrow.
type presigner interface {
	PresignedGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	PresignedPut(ctx context.Context, key, contentType string, ttl time.Duration) (string, error)
}

// Storage is the objectStore adapter. It presigns via lib/storage and moves the bytes with
// an injected http.Client (its own timeout governs the round-trip).
type Storage struct {
	presign presigner
	http    *http.Client
}

// NewStorage wires the adapter to lib/storage's client and an http client. A nil http
// client falls back to a sane defaulted one so the worker can inject just the presigner.
func NewStorage(presign presigner, httpClient *http.Client) *Storage {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 2 * time.Minute}
	}
	return &Storage{presign: presign, http: httpClient}
}

// Get fetches the object bytes: presign a GET, then HTTP GET the URL and read the body. A
// non-2xx or a transport fault is a typed infra error (retryable); the caller never sees a
// raw *http.Response.
func (s *Storage) Get(ctx context.Context, key string) ([]byte, error) {
	url, err := s.presign.PresignedGet(ctx, key, presignedTTL)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, apperr.NewInfra("extraction: build get request", err)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, apperr.NewInfra("extraction: get object", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, apperr.NewInfra("extraction: read object body", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, apperr.NewInfra("extraction: get object bad status "+resp.Status, nil)
	}
	return body, nil
}

// Put writes the object bytes: presign a PUT for contentType, then HTTP PUT the body with a
// matching Content-Type (the type is signed into the URL, so it must match). A non-2xx or a
// transport fault is a typed infra error.
func (s *Storage) Put(ctx context.Context, key string, body []byte, contentType string) error {
	url, err := s.presign.PresignedPut(ctx, key, contentType, presignedTTL)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return apperr.NewInfra("extraction: build put request", err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := s.http.Do(req)
	if err != nil {
		return apperr.NewInfra("extraction: put object", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return apperr.NewInfra("extraction: put object bad status "+resp.Status, nil)
	}
	return nil
}
