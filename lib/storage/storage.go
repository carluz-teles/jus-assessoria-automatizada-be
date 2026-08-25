// Package storage is an S3-compatible object storage client that speaks to AWS
// S3, Cloudflare R2, or MinIO through the same surface. It is infra, not domain:
// slices inject a *Client and never see the AWS SDK directly.
//
// The upload contract lives in docs/erd-backend.md §4e.4 — files never transit
// the backend. The client hands out short-lived presigned PUT URLs (upload) and
// presigned GET URLs (download); the byte stream goes straight to the bucket.
// Every object key is tenant-scoped ({tenant}/{prefix}/{uuid}) so isolation
// holds at the storage layer too, not only in the database.
package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/apperr"
)

// Options carries everything New needs to reach a bucket. Endpoint and
// UsePathStyle exist for S3-compatible providers: leave Endpoint empty for real
// AWS (the SDK resolves the regional endpoint), set it for R2/MinIO. MinIO also
// needs UsePathStyle=true (host/bucket/key instead of bucket.host/key).
type Options struct {
	Endpoint     string // empty = real AWS; set for R2/MinIO
	Region       string
	Bucket       string
	AccessKey    string
	SecretKey    string
	UsePathStyle bool // true for MinIO path-style addressing
}

// Client presigns object URLs and confirms object existence for a single
// bucket. It is safe for concurrent use — the underlying s3 clients are.
type Client struct {
	bucket  string
	api     *s3.Client
	presign *s3.PresignClient
}

// New validates the required fields and builds a client bound to opts.Bucket.
// Missing required config is a caller mistake, so it returns an apperr.Invalid
// (not Infra) — construction never touches the network here.
func New(_ context.Context, opts Options) (*Client, error) {
	if opts.Bucket == "" {
		return nil, apperr.NewInvalid("storage: bucket is required")
	}
	if opts.Region == "" {
		return nil, apperr.NewInvalid("storage: region is required")
	}
	if opts.AccessKey == "" {
		return nil, apperr.NewInvalid("storage: access key is required")
	}
	if opts.SecretKey == "" {
		return nil, apperr.NewInvalid("storage: secret key is required")
	}

	cfg := aws.Config{
		Region:      opts.Region,
		Credentials: credentials.NewStaticCredentialsProvider(opts.AccessKey, opts.SecretKey, ""),
	}

	// Copy Endpoint into a local so the pointer we hand the SDK does not alias
	// the caller's Options value.
	endpoint := opts.Endpoint
	api := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = &endpoint
		}
		o.UsePathStyle = opts.UsePathStyle
	})

	return &Client{
		bucket:  opts.Bucket,
		api:     api,
		presign: s3.NewPresignClient(api),
	}, nil
}

// PresignedPut returns a short-lived URL the client uses to upload the object
// directly to storage (step 1 of the upload flow). contentType is signed into
// the URL, so the client must send the same Content-Type on the PUT.
func (c *Client) PresignedPut(ctx context.Context, key, contentType string, ttl time.Duration) (string, error) {
	req, err := c.presign.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:      &c.bucket,
		Key:         &key,
		ContentType: &contentType,
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", apperr.NewInfra("storage: presign put", err)
	}

	return req.URL, nil
}

// PresignedGet returns a short-lived URL for downloading the object. Callers
// MUST confirm the requester's tenant matches the object's tenant (the key
// prefix) before handing this out — the URL itself carries no such check.
func (c *Client) PresignedGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	req, err := c.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: &c.bucket,
		Key:    &key,
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", apperr.NewInfra("storage: presign get", err)
	}

	return req.URL, nil
}

// Exists confirms an object is present via a HEAD request — step 3 of the
// upload flow, before a record is created pointing at the key. A NotFound is a
// normal "not uploaded yet" answer (false, nil); any other failure is infra.
//
// This hits the network, so unit tests skip it; the integration slice covers it
// against a real bucket.
func (c *Client) Exists(ctx context.Context, key string) (bool, error) {
	_, err := c.api.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: &c.bucket,
		Key:    &key,
	})
	if err == nil {
		return true, nil
	}

	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return false, nil
	}

	return false, apperr.NewInfra("storage: head object", err)
}

// NewKey builds a tenant-scoped object key: {tenantID}/{prefix}/{uuid}. The
// tenant segment keeps objects isolated per §4e.4; the random uuid makes the
// key unguessable and collision-free.
func NewKey(tenantID, prefix string) string {
	return tenantID + "/" + prefix + "/" + uuid.NewString()
}

// PutBytes uploads bytes DIRECTLY through the backend — used when the object
// MUST be inspected/transformed server-side before persisting (ex.: certificate
// upload: parse PKCS#12, extract metadata, encrypt with master key, then store).
// For user-facing file uploads keep using PresignedPut so bytes never transit
// the API.
func (c *Client) PutBytes(ctx context.Context, key, contentType string, data []byte) error {
	_, err := c.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      &c.bucket,
		Key:         &key,
		ContentType: &contentType,
		Body:        bytes.NewReader(data),
	})
	if err != nil {
		return apperr.NewInfra("storage: put object", err)
	}
	return nil
}

// GetBytes downloads an object server-side (bypasses presigned GET). Same
// rationale as PutBytes: used only when the API MUST touch the bytes (ex.:
// decrypt + open a certificate PKCS#12 to sign). NotFound surfaces as a
// tenant-agnostic infra error — the caller layered the tenant guard already.
func (c *Client) GetBytes(ctx context.Context, key string) ([]byte, error) {
	out, err := c.api.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &c.bucket,
		Key:    &key,
	})
	if err != nil {
		return nil, apperr.NewInfra("storage: get object", err)
	}
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, apperr.NewInfra("storage: read object body", err)
	}
	return data, nil
}

// Delete removes an object. Idempotent from the caller's perspective — a
// missing key still returns nil (S3 semantics). Used when a certificate is
// hard-removed (fatia 2), NOT for the soft-delete revoke path.
func (c *Client) Delete(ctx context.Context, key string) error {
	_, err := c.api.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: &c.bucket,
		Key:    &key,
	})
	if err != nil {
		return apperr.NewInfra("storage: delete object", err)
	}
	return nil
}
