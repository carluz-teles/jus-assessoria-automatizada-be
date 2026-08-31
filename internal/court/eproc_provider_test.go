package court

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jusassessoria/platform/lib/eproc"
)

// fakeDocumentWriter is an in-memory DocumentWriter — records every
// WriteDocument call and can be told to fail for a specific title, so tests
// can prove a single write failure doesn't abort the rest of the batch.
type fakeDocumentWriter struct {
	failTitles map[string]bool
	calls      []fakeDocumentWrite
}

type fakeDocumentWrite struct {
	tenantID, courtRecordID, mimeType, checksum, title string
}

func (w *fakeDocumentWriter) WriteDocument(_ context.Context, tenantID, courtRecordID, mimeType, checksum, title string, _ []byte) (string, error) {
	w.calls = append(w.calls, fakeDocumentWrite{
		tenantID:      tenantID,
		courtRecordID: courtRecordID,
		mimeType:      mimeType,
		checksum:      checksum,
		title:         title,
	})
	if w.failTitles[title] {
		return "", assert.AnError
	}
	return "doc-" + title, nil
}

// eprocDocumentStub serves /api/documento/<id> — 200+bytes for known ids, 404
// for any other (the download-failure case downloadOneDocument must tolerate).
func eprocDocumentStub(t *testing.T, docs map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/documento/")
		body, ok := docs[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// sessionCookieDTO mirrors lib/eproc's own (unexported) session.go wire shape
// ({"host":...,"cookies":[...]}) — duck-typed here since Session is just an
// opaque []byte and this is the only way a caller outside the package can
// prime one to skip login and go straight to an authenticated request.
type sessionCookieDTO struct {
	Host    string         `json:"host"`
	Cookies []*http.Cookie `json:"cookies"`
}

func fakeConnectedSession(t *testing.T, baseURL string) eproc.Session {
	t.Helper()
	b, err := json.Marshal([]sessionCookieDTO{
		{Host: baseURL, Cookies: []*http.Cookie{{Name: "eproc_session", Value: "primed"}}},
	})
	require.NoError(t, err)
	return eproc.Session(b)
}

func newPrimedEprocClient(t *testing.T, baseURL string) *eproc.Client {
	t.Helper()
	return eproc.NewEprocClient(nil, eproc.WithBaseURL(baseURL), eproc.WithSession(fakeConnectedSession(t, baseURL)))
}

func TestEprocProvider_downloadNewDocuments_SkipsEventsAtOrBeforeCursor(t *testing.T) {
	srv := eprocDocumentStub(t, map[string]string{"doc-new": "%PDF new"})
	client := newPrimedEprocClient(t, srv.URL)
	writer := &fakeDocumentWriter{failTitles: map[string]bool{}}
	p := NewEprocProvider(nil, writer)

	cursor := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	events := []eproc.Event{
		{
			ExternalID: "ev-old",
			Date:       cursor, // NOT strictly after cursor — already processed last time
			Documents:  []eproc.DocumentRef{{ExternalID: "doc-old", DownloadPath: "/api/documento/doc-old", Label: "old", MIMEType: "application/pdf"}},
		},
		{
			ExternalID: "ev-new",
			Date:       cursor.Add(time.Hour),
			Documents:  []eproc.DocumentRef{{ExternalID: "doc-new", DownloadPath: "/api/documento/doc-new", Label: "new", MIMEType: "application/pdf"}},
		},
	}

	downloaded := p.downloadNewDocuments(context.Background(), client, "tenant-1", "record-1", events, cursor)

	assert.Equal(t, 1, downloaded)
	require.Len(t, writer.calls, 1)
	assert.Equal(t, "new", writer.calls[0].title)
	assert.Equal(t, "tenant-1", writer.calls[0].tenantID)
	assert.Equal(t, "record-1", writer.calls[0].courtRecordID)
}

func TestEprocProvider_downloadNewDocuments_ZeroCursorDownloadsEverything(t *testing.T) {
	srv := eprocDocumentStub(t, map[string]string{"doc-a": "a", "doc-b": "b"})
	client := newPrimedEprocClient(t, srv.URL)
	writer := &fakeDocumentWriter{failTitles: map[string]bool{}}
	p := NewEprocProvider(nil, writer)

	events := []eproc.Event{
		{ExternalID: "ev1", Date: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Documents: []eproc.DocumentRef{{ExternalID: "doc-a", DownloadPath: "/api/documento/doc-a", Label: "a"}}},
		{ExternalID: "ev2", Date: time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC), Documents: []eproc.DocumentRef{{ExternalID: "doc-b", DownloadPath: "/api/documento/doc-b", Label: "b"}}},
	}

	downloaded := p.downloadNewDocuments(context.Background(), client, "tenant-1", "record-1", events, time.Time{})

	assert.Equal(t, 2, downloaded)
	assert.Len(t, writer.calls, 2)
}

func TestEprocProvider_downloadNewDocuments_OneDocumentFailureDoesNotAbortTheRest(t *testing.T) {
	srv := eprocDocumentStub(t, map[string]string{"doc-good": "%PDF good", "doc-writer-fails": "%PDF writer-fails"})
	client := newPrimedEprocClient(t, srv.URL)
	writer := &fakeDocumentWriter{failTitles: map[string]bool{"writer-fails": true}}
	p := NewEprocProvider(nil, writer)

	cursor := time.Time{}
	events := []eproc.Event{
		{
			ExternalID: "ev1",
			Date:       time.Now(),
			Documents: []eproc.DocumentRef{
				{ExternalID: "doc-good", DownloadPath: "/api/documento/doc-good", Label: "good", MIMEType: "application/pdf"},
				{ExternalID: "doc-missing-on-portal", DownloadPath: "/api/documento/doc-missing-on-portal", Label: "missing-on-portal", MIMEType: "application/pdf"},
				{ExternalID: "doc-writer-fails", DownloadPath: "/api/documento/doc-writer-fails", Label: "writer-fails", MIMEType: "application/pdf"},
			},
		},
	}

	downloaded := p.downloadNewDocuments(context.Background(), client, "tenant-1", "record-1", events, cursor)

	// Only "good" survives end-to-end: "missing-on-portal" never downloads (404),
	// "writer-fails" downloads fine but DocumentWriter rejects it.
	assert.Equal(t, 1, downloaded)
	require.Len(t, writer.calls, 2) // good + writer-fails both REACHED the writer
	titles := []string{writer.calls[0].title, writer.calls[1].title}
	assert.ElementsMatch(t, []string{"good", "writer-fails"}, titles)
}

func TestEprocProvider_downloadNewDocuments_NilDocWriterIsNoOp(t *testing.T) {
	p := NewEprocProvider(nil, nil)

	events := []eproc.Event{
		{ExternalID: "ev1", Date: time.Now(), Documents: []eproc.DocumentRef{{ExternalID: "doc-a", Label: "a"}}},
	}

	downloaded := p.downloadNewDocuments(context.Background(), nil, "tenant-1", "record-1", events, time.Time{})

	assert.Equal(t, 0, downloaded)
}
