package extraction

import (
	"context"
	"net/http"
)

// storage.go is the objectStore adapter over lib/storage. This slice runs SERVER-SIDE in the
// worker (the trusted key holder, not a browser), so it reads/writes the bytes DIRECTLY through
// the backend (lib/storage's GetBytes/PutBytes) rather than presigning a URL and doing an HTTP
// round-trip. Direct access is the correct server-side path AND it sidesteps an aws-sdk-go-v2
// quirk where presigned URLs ignore UsePathStyle and come out virtual-hosted (bucket.host/key),
// which has no DNS against MinIO — the object client honors path-style, the presigner doesn't.

// backendStore is the slice of lib/storage.Client the adapter needs — direct byte access.
// Depending on it (not the concrete *storage.Client) keeps the adapter testable and the
// coupling narrow. (Named distinctly from the use case's own objectStore port, which is the
// Get/Put surface THIS adapter provides.)
type backendStore interface {
	GetBytes(ctx context.Context, key string) ([]byte, error)
	PutBytes(ctx context.Context, key, contentType string, data []byte) error
}

// Storage is the objectStore adapter: a thin pass-through to lib/storage's direct byte access.
type Storage struct {
	store backendStore
}

// NewStorage wires the adapter to lib/storage's client. The second arg (an *http.Client) is
// retained for call-site compatibility but unused now that reads/writes go direct (no presigned
// HTTP round-trip).
func NewStorage(store backendStore, _ *http.Client) *Storage {
	return &Storage{store: store}
}

// Get fetches the object bytes directly from the backend.
func (s *Storage) Get(ctx context.Context, key string) ([]byte, error) {
	return s.store.GetBytes(ctx, key)
}

// Put writes the object bytes directly to the backend.
func (s *Storage) Put(ctx context.Context, key string, body []byte, contentType string) error {
	return s.store.PutBytes(ctx, key, contentType, body)
}
