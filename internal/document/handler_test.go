package document

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/httpx"
	"github.com/jusassessoria/platform/lib/httpx/middleware"
)

// --- HTTP test doubles -------------------------------------------------------

// stubVerifier accepts any bearer token — Auth's job here is only to gate on the token's
// presence, not to test Clerk.
type stubVerifier struct{}

func (stubVerifier) Verify(context.Context, string) (userID, orgID, role string, err error) {
	return "clerk-user", "clerk-org", "", nil
}

// stubResolver returns a principal with the configured tenant, standing in for the identity
// slice's resolver.
type stubResolver struct{ tenant string }

func (r stubResolver) Resolve(context.Context, string, string) (httpx.Principal, error) {
	return httpx.Principal{UserID: "u-1", TenantID: r.tenant, Role: "LAWYER"}, nil
}

// recordingReader implements the handler's reader port, capturing the queries the handler
// forwards and returning canned results/errors.
type recordingReader struct {
	byProcRes  DocumentsByProcessoResult
	gotByProcQ DocumentsByProcessoQuery

	detailView   DocumentView
	detailErr    error
	gotDetailTID string
	gotDetailID  string
}

func (r *recordingReader) DocumentsByProcesso(_ context.Context, q DocumentsByProcessoQuery) (DocumentsByProcessoResult, error) {
	r.gotByProcQ = q
	return r.byProcRes, nil
}

func (r *recordingReader) Document(_ context.Context, tenantID, id string) (DocumentView, error) {
	r.gotDetailTID, r.gotDetailID = tenantID, id
	return r.detailView, r.detailErr
}

// recordingWriter implements the handler's writer port, capturing the commands the handler
// forwards and returning canned results/errors for each write entry point.
type recordingWriter struct {
	gotStartCmd StartUploadCommand
	startRes    StartUploadResult
	startErr    error

	gotCompleteCmd CompleteCommand
	completeView   DocumentView
	completeErr    error

	gotDownloadTID, gotDownloadID string
	downloadRes                   DownloadResult
	downloadErr                   error

	gotDeleteTID, gotDeleteID string
	deleteErr                 error
}

func (w *recordingWriter) Start(_ context.Context, cmd StartUploadCommand) (StartUploadResult, error) {
	w.gotStartCmd = cmd
	return w.startRes, w.startErr
}

func (w *recordingWriter) Complete(_ context.Context, cmd CompleteCommand) (DocumentView, error) {
	w.gotCompleteCmd = cmd
	return w.completeView, w.completeErr
}

func (w *recordingWriter) Download(_ context.Context, tenantID, id string) (DownloadResult, error) {
	w.gotDownloadTID, w.gotDownloadID = tenantID, id
	return w.downloadRes, w.downloadErr
}

func (w *recordingWriter) Delete(_ context.Context, tenantID, id string) error {
	w.gotDeleteTID, w.gotDeleteID = tenantID, id
	return w.deleteErr
}

// newApp builds a Fiber app with the tenant-auth middleware and the document routes mounted,
// standing in for the api's composition. The stub resolver pins the tenant so every handler
// reads it from the principal.
func newApp(rd reader, wr writer, tenant string) *fiber.App {
	app := fiber.New(fiber.Config{
		ErrorHandler: func(c *fiber.Ctx, err error) error { return httpx.WriteError(c, err) },
	})
	v1 := app.Group("/v1", middleware.Auth(stubVerifier{}, stubResolver{tenant: tenant}))
	NewHandler(rd, wr).RegisterV1(v1)
	return app
}

func do(t *testing.T, app *fiber.App, method, path, body string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	req.Header.Set("Authorization", "Bearer t")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test(%s %s) error = %v", method, path, err)
	}
	return resp
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return out
}

// --- tests ------------------------------------------------------------------

// TestStartUpload_Valid proves a well-formed POST /v1/documentos returns 201 with the presign
// response and forwards the command with the tenant from the principal (never the body).
func TestStartUpload_Valid(t *testing.T) {
	crid := uuid.NewString()
	wr := &recordingWriter{startRes: StartUploadResult{
		DocumentID: "doc-1", UploadURL: "https://s3/put", StorageKey: "tenant-9/documents/k", ExpiresIn: 900,
	}}
	app := newApp(&recordingReader{}, wr, "tenant-9")

	body := `{"court_record_id":"` + crid + `","document_type":"PETICAO","original_filename":"peca.pdf","mime_type":"application/pdf","size_bytes":1024}`
	resp := do(t, app, http.MethodPost, "/v1/documentos", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	got := decode[startUploadResponse](t, resp)
	if got.DocumentID != "doc-1" || got.UploadURL != "https://s3/put" || got.ExpiresIn != 900 {
		t.Errorf("response = %+v", got)
	}
	if wr.gotStartCmd.TenantID != "tenant-9" {
		t.Errorf("tenant = %q, want tenant-9 (from principal)", wr.gotStartCmd.TenantID)
	}
	if wr.gotStartCmd.CourtRecordID != crid || wr.gotStartCmd.SizeBytes != 1024 {
		t.Errorf("cmd = %+v", wr.gotStartCmd)
	}
}

// TestStartUpload_MissingFields rejects a body without the required metadata (400) before the
// writer is called.
func TestStartUpload_MissingFields(t *testing.T) {
	wr := &recordingWriter{}
	app := newApp(&recordingReader{}, wr, "tenant-9")

	resp := do(t, app, http.MethodPost, "/v1/documentos", `{"document_type":"PETICAO"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if wr.gotStartCmd.TenantID != "" {
		t.Errorf("writer was called on an invalid body")
	}
}

// TestStartUpload_BadCourtRecordID rejects a malformed optional court_record_id (400).
func TestStartUpload_BadCourtRecordID(t *testing.T) {
	app := newApp(&recordingReader{}, &recordingWriter{}, "tenant-9")
	body := `{"court_record_id":"not-a-uuid","document_type":"PETICAO","original_filename":"a.pdf","mime_type":"application/pdf","size_bytes":10}`
	resp := do(t, app, http.MethodPost, "/v1/documentos", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestCompleteUpload_ForwardsCommand proves POST /v1/documentos/:id/complete forwards the id +
// tenant + optional checksum and returns 200 with the view.
func TestCompleteUpload_ForwardsCommand(t *testing.T) {
	wr := &recordingWriter{completeView: DocumentView{ID: "doc-1", Status: "UPLOADED"}}
	app := newApp(&recordingReader{}, wr, "tenant-9")

	resp := do(t, app, http.MethodPost, "/v1/documentos/doc-1/complete", `{"checksum":"abc123"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := decode[DocumentView](t, resp)
	if got.Status != "UPLOADED" {
		t.Errorf("view status = %q", got.Status)
	}
	if wr.gotCompleteCmd.DocumentID != "doc-1" || wr.gotCompleteCmd.TenantID != "tenant-9" || wr.gotCompleteCmd.Checksum != "abc123" {
		t.Errorf("cmd = %+v", wr.gotCompleteCmd)
	}
}

// TestCompleteUpload_NoBody proves an empty body is accepted (checksum optional) and still
// forwards the command.
func TestCompleteUpload_NoBody(t *testing.T) {
	wr := &recordingWriter{completeView: DocumentView{ID: "doc-1", Status: "UPLOADED"}}
	app := newApp(&recordingReader{}, wr, "tenant-9")

	resp := do(t, app, http.MethodPost, "/v1/documentos/doc-1/complete", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if wr.gotCompleteCmd.DocumentID != "doc-1" || wr.gotCompleteCmd.Checksum != "" {
		t.Errorf("cmd = %+v", wr.gotCompleteCmd)
	}
}

// TestCompleteUpload_PropagatesConflict maps the writer's ErrDocumentNotUploadable to 409.
func TestCompleteUpload_PropagatesConflict(t *testing.T) {
	wr := &recordingWriter{completeErr: ErrDocumentNotUploadable}
	app := newApp(&recordingReader{}, wr, "tenant-9")

	resp := do(t, app, http.MethodPost, "/v1/documentos/doc-1/complete", "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}

// TestDownload_ForwardsAndRenders proves GET /v1/documentos/:id/download returns 200 with the
// presigned URL and forwards the id + tenant.
func TestDownload_ForwardsAndRenders(t *testing.T) {
	wr := &recordingWriter{downloadRes: DownloadResult{URL: "https://s3/get", ExpiresIn: 300}}
	app := newApp(&recordingReader{}, wr, "tenant-9")

	resp := do(t, app, http.MethodGet, "/v1/documentos/doc-1/download", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := decode[downloadResponse](t, resp)
	if got.URL != "https://s3/get" || got.ExpiresIn != 300 {
		t.Errorf("response = %+v", got)
	}
	if wr.gotDownloadID != "doc-1" || wr.gotDownloadTID != "tenant-9" {
		t.Errorf("download id/tenant = %q/%q", wr.gotDownloadID, wr.gotDownloadTID)
	}
}

// TestListByProcesso_FirstPage proves GET /v1/processos/:id/documentos forwards the process id +
// tenant with the max sentinel cursor and wraps the result in the page envelope.
func TestListByProcesso_FirstPage(t *testing.T) {
	rd := &recordingReader{byProcRes: DocumentsByProcessoResult{
		Items: []DocumentView{{ID: "doc-1", Status: "UPLOADED"}}, Total: 1,
	}}
	app := newApp(rd, &recordingWriter{}, "tenant-9")

	resp := do(t, app, http.MethodGet, "/v1/processos/proc-1/documentos", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if rd.gotByProcQ.CourtRecordID != "proc-1" || rd.gotByProcQ.TenantID != "tenant-9" {
		t.Errorf("query = %+v", rd.gotByProcQ)
	}
	if rd.gotByProcQ.LastID != maxUUID {
		t.Errorf("first-page cursor id = %q, want max sentinel", rd.gotByProcQ.LastID)
	}
	page := decode[httpx.Page[DocumentView]](t, resp)
	if len(page.Data) != 1 || page.Page.Total != 1 || page.Page.NextCursor != nil {
		t.Errorf("page = %+v", page)
	}
}

// TestListByProcesso_NextCursor proves a HasMore result emits a next_cursor keyed off the last
// row's (created_at, id).
func TestListByProcesso_NextCursor(t *testing.T) {
	rd := &recordingReader{byProcRes: DocumentsByProcessoResult{
		Items: []DocumentView{{ID: uuid.NewString(), Status: "UPLOADED"}}, HasMore: true, Total: 5,
	}}
	app := newApp(rd, &recordingWriter{}, "tenant-9")

	resp := do(t, app, http.MethodGet, "/v1/processos/proc-1/documentos", "")
	page := decode[httpx.Page[DocumentView]](t, resp)
	if page.Page.NextCursor == nil {
		t.Fatalf("want a next_cursor when HasMore")
	}
	cur, err := httpx.DecodeCursor(*page.Page.NextCursor)
	if err != nil {
		t.Fatalf("decode next cursor: %v", err)
	}
	if cur.LastID != rd.byProcRes.Items[0].ID {
		t.Errorf("cursor last_id = %q, want %q", cur.LastID, rd.byProcRes.Items[0].ID)
	}
}

// TestGetDocument_NotFound maps the reader's ErrDocumentNotFound to 404.
func TestGetDocument_NotFound(t *testing.T) {
	rd := &recordingReader{detailErr: ErrDocumentNotFound}
	app := newApp(rd, &recordingWriter{}, "tenant-9")

	resp := do(t, app, http.MethodGet, "/v1/documentos/doc-x", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	if rd.gotDetailID != "doc-x" || rd.gotDetailTID != "tenant-9" {
		t.Errorf("detail id/tenant = %q/%q", rd.gotDetailID, rd.gotDetailTID)
	}
}

// TestDeleteDocument_NoContent proves DELETE /v1/documentos/:id returns 204 and forwards the id +
// tenant.
func TestDeleteDocument_NoContent(t *testing.T) {
	wr := &recordingWriter{}
	app := newApp(&recordingReader{}, wr, "tenant-9")

	resp := do(t, app, http.MethodDelete, "/v1/documentos/doc-1", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if wr.gotDeleteID != "doc-1" || wr.gotDeleteTID != "tenant-9" {
		t.Errorf("delete id/tenant = %q/%q", wr.gotDeleteID, wr.gotDeleteTID)
	}
}

// TestDeleteDocument_Conflict maps the writer's ErrDocumentNotDeletable (origin=COURT) to 409.
func TestDeleteDocument_Conflict(t *testing.T) {
	wr := &recordingWriter{deleteErr: ErrDocumentNotDeletable}
	app := newApp(&recordingReader{}, wr, "tenant-9")

	resp := do(t, app, http.MethodDelete, "/v1/documentos/doc-1", "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}
