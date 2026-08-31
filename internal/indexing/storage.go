package indexing

import (
	"context"
	"net/http"
)

// storage.go is the concrete objectReader — it reads an object's bytes from S3-compatible
// storage. The worker is the trusted key holder (server-side), so it reads DIRECTLY through the
// backend (lib/storage's GetBytes) rather than presigning a URL and doing an HTTP round-trip.
// Direct access is the correct server-side path AND avoids an aws-sdk-go-v2 quirk where presigned
// URLs ignore UsePathStyle and come out virtual-hosted (no DNS against MinIO). Kept behind the
// objectReader port so the pipeline reads canned bytes in tests without touching storage.

// objectByteReader is the sliver of lib/storage.Client this adapter needs: a direct byte read.
// Depending on it (not the concrete *storage.Client) keeps construction lib-agnostic and testable.
type objectByteReader interface {
	GetBytes(ctx context.Context, key string) ([]byte, error)
}

// storageReader reads objects directly from the backend.
type storageReader struct {
	storage objectByteReader
}

var _ objectReader = (*storageReader)(nil)

// NewStorageReader builds the objectReader from the storage client. The second arg (an
// *http.Client) is retained for call-site compatibility but unused (reads go direct now).
func NewStorageReader(storage objectByteReader, _ *http.Client) objectReader {
	return &storageReader{storage: storage}
}

// ReadObject reads the object bytes directly. A fault is an infra error (retryable — a transient
// storage blip); the caller (the pipeline) keeps these on asynq's retry path.
func (r *storageReader) ReadObject(ctx context.Context, key string) ([]byte, error) {
	return r.storage.GetBytes(ctx, key)
}
