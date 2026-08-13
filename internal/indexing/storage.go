package indexing

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jusassessoria/platform/lib/apperr"
)

// storage.go is the concrete objectReader — it reads an object's bytes from S3-compatible
// storage. lib/storage only presigns URLs (no direct object read), so this presigns a short-lived
// GET and http.Gets it. Kept behind the objectReader port so the pipeline reads canned bytes in
// tests without touching storage or the network.

// presignGetTTL is how long the internal presigned GET is valid — a few minutes is ample for the
// worker's own immediate read (this URL never leaves the process).
const presignGetTTL = 5 * time.Minute

// presigner is the sliver of lib/storage.Client this adapter needs: presign a GET URL. Depending
// on it (not the concrete *storage.Client) keeps the adapter's own construction lib-agnostic and
// unit-testable.
type presigner interface {
	PresignedGet(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// storageReader reads objects by presigning a GET and fetching it. It holds the presigner (the
// storage client) and an http.Client (injectable; nil → http.DefaultClient).
type storageReader struct {
	storage presigner
	client  *http.Client
}

var _ objectReader = (*storageReader)(nil)

// NewStorageReader builds the objectReader from the storage client. client nil → http.DefaultClient.
func NewStorageReader(storage presigner, client *http.Client) objectReader {
	if client == nil {
		client = http.DefaultClient
	}
	return &storageReader{storage: storage, client: client}
}

// ReadObject presigns a short-lived GET for key and fetches the bytes. A presign failure or a
// non-2xx / body-read fault is an infra error (retryable — a transient storage blip); the caller
// (the pipeline) keeps these on asynq's retry path.
func (r *storageReader) ReadObject(ctx context.Context, key string) ([]byte, error) {
	url, err := r.storage.PresignedGet(ctx, key, presignGetTTL)
	if err != nil {
		return nil, err // already a typed apperr from lib/storage
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, apperr.NewInfra("indexing: build storage request", err)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, apperr.NewInfra("indexing: storage request failed", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, apperr.NewInfra("indexing: read storage object", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, apperr.NewInfra("indexing: storage unexpected status", fmt.Errorf("status %d for key %s", resp.StatusCode, key))
	}
	return body, nil
}
