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
	tenantID, courtRecordID, mimeType, checksum, title, documentType string
}

func (w *fakeDocumentWriter) WriteDocument(_ context.Context, tenantID, courtRecordID, mimeType, checksum, title, documentType string, _ []byte) (string, error) {
	w.calls = append(w.calls, fakeDocumentWrite{
		tenantID:      tenantID,
		courtRecordID: courtRecordID,
		mimeType:      mimeType,
		checksum:      checksum,
		title:         title,
		documentType:  documentType,
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

// fakePartyWriter is an in-memory PartyWriter — records the last UpsertParties call
// and can be told to fail, so tests can prove writeParties maps the eproc parties
// faithfully and swallows a writer error (best-effort enrichment, never fails the fetch).
type fakePartyWriter struct {
	fail    bool
	calls   int
	lastCtx context.Context //nolint:containedctx // test capture only
	lastTID string
	lastCRI string
	last    []ProcessParty
}

func (w *fakePartyWriter) UpsertParties(ctx context.Context, tenantID, courtRecordID string, parties []ProcessParty) error {
	w.calls++
	w.lastCtx = ctx
	w.lastTID = tenantID
	w.lastCRI = courtRecordID
	w.last = parties
	if w.fail {
		return assert.AnError
	}
	return nil
}

func TestEprocProvider_writeParties_MapsPartiesAndCounsels(t *testing.T) {
	writer := &fakePartyWriter{}
	p := NewEprocProvider(nil, nil, WithPartyWriter(writer))

	proc := &eproc.Process{Parties: []eproc.Party{
		{
			Role:     "AUTOR",
			Name:     "MURILO DE PAULA BALDAN",
			Document: "284.669.278-59",
			Counsels: []eproc.Counsel{{Name: "PAULO SOUZA", OAB: "321511", UF: "SP"}},
		},
		{
			Role:     "REU",
			Name:     "RITA MARCIA MONTEIRO SEZEFREDO",
			Document: "",
			Counsels: nil,
		},
	}}

	p.writeParties(context.Background(), "tenant-1", "record-1", proc)

	require.Equal(t, 1, writer.calls)
	assert.Equal(t, "tenant-1", writer.lastTID)
	assert.Equal(t, "record-1", writer.lastCRI)
	require.Len(t, writer.last, 2)

	assert.Equal(t, ProcessParty{
		Role:     "AUTOR",
		Name:     "MURILO DE PAULA BALDAN",
		Document: "284.669.278-59",
		Counsels: []ProcessCounsel{{Name: "PAULO SOUZA", OAB: "321511", UF: "SP"}},
	}, writer.last[0])

	assert.Equal(t, "REU", writer.last[1].Role)
	assert.Empty(t, writer.last[1].Document)
	assert.Empty(t, writer.last[1].Counsels)
}

func TestEprocProvider_writeParties_NilWriterAndEmptyPartiesAreNoOp(t *testing.T) {
	// nil writer: must not panic.
	NewEprocProvider(nil, nil).writeParties(context.Background(), "t", "r", &eproc.Process{
		Parties: []eproc.Party{{Role: "AUTOR", Name: "X"}},
	})

	// empty parties: writer is never called.
	writer := &fakePartyWriter{}
	p := NewEprocProvider(nil, nil, WithPartyWriter(writer))
	p.writeParties(context.Background(), "t", "r", &eproc.Process{Parties: nil})
	assert.Equal(t, 0, writer.calls)
}

func TestEprocProvider_writeParties_WriterErrorIsSwallowed(t *testing.T) {
	writer := &fakePartyWriter{fail: true}
	p := NewEprocProvider(nil, nil, WithPartyWriter(writer))

	// No panic, no propagation — writeParties returns nothing; the fetch is not failed.
	p.writeParties(context.Background(), "t", "r", &eproc.Process{
		Parties: []eproc.Party{{Role: "AUTOR", Name: "X"}},
	})
	assert.Equal(t, 1, writer.calls)
}

func TestDocTitleAndType(t *testing.T) {
	tests := []struct {
		name          string
		code          string
		eventDesc     string
		wantTitle     string
		wantType      string
	}{
		{
			name:      "known code maps to friendly label; type keeps the raw code",
			code:      "SENT",
			eventDesc: "Sentença registrada",
			wantTitle: "Sentença",
			wantType:  "SENT",
		},
		{
			name:      "unknown code falls back to the event description",
			code:      "XPTO",
			eventDesc: "Despacho saneador",
			wantTitle: "Despacho saneador",
			wantType:  "XPTO",
		},
		{
			name:      "unknown code with no description falls back to the raw code",
			code:      "XPTO",
			eventDesc: "",
			wantTitle: "XPTO",
			wantType:  "XPTO",
		},
		{
			name:      "lowercase code is matched case-insensitively",
			code:      "contrsocial",
			eventDesc: "",
			wantTitle: "Contrato social",
			wantType:  "contrsocial",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			is := assert.New(t)
			title, documentType := docTitleAndType(tt.code, tt.eventDesc)
			is.Equal(tt.wantTitle, title)
			is.Equal(tt.wantType, documentType)
		})
	}
}
