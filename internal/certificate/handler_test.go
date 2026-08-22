package certificate

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jusassessoria/platform/lib/httpx"
)

// stubUC implements the handler's uc port, capturing commands and returning canned
// results/errors per endpoint.
type stubUC struct {
	createRes CertificateView
	createErr error
	gotCreate CreateCommand

	previewRes PreviewResult
	previewErr error
	gotPreview PreviewCommand

	listRes []CertificateView
	listErr error

	revokeErr error
	gotRevTID string
	gotRevID  string
}

func (s *stubUC) Create(_ context.Context, cmd CreateCommand) (CertificateView, error) {
	s.gotCreate = cmd
	return s.createRes, s.createErr
}

func (s *stubUC) Preview(_ context.Context, cmd PreviewCommand) (PreviewResult, error) {
	s.gotPreview = cmd
	return s.previewRes, s.previewErr
}

func (s *stubUC) List(_ context.Context, _ string) ([]CertificateView, error) {
	return s.listRes, s.listErr
}

func (s *stubUC) Revoke(_ context.Context, tenantID, id string) error {
	s.gotRevTID, s.gotRevID = tenantID, id
	return s.revokeErr
}

// newTestApp mounts the certificate routes behind a middleware that injects a fixed
// principal (standing in for the resolved Clerk auth), so the handler is tested in
// isolation from the auth chain.
func newTestApp(h *Handler) *fiber.App {
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		httpx.SetPrincipal(c, httpx.Principal{UserID: "owner-1", TenantID: "tenant-1", Role: "LAWYER"})
		return c.Next()
	})
	h.Register(app.Group("/v1"))
	return app
}

// multipartPFX builds a multipart body with the file + password fields.
func multipartPFX(t *testing.T, fileBytes []byte, password string) (body *bytes.Buffer, contentType string) {
	t.Helper()
	body = &bytes.Buffer{}
	w := multipart.NewWriter(body)
	if fileBytes != nil {
		fw, err := w.CreateFormFile("file", "cert.pfx")
		require.NoError(t, err)
		_, err = fw.Write(fileBytes)
		require.NoError(t, err)
	}
	if password != "" {
		require.NoError(t, w.WriteField("password", password))
	}
	require.NoError(t, w.Close())
	return body, w.FormDataContentType()
}

func TestHandler_Create_201(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	stub := &stubUC{createRes: CertificateView{ID: "c1", SubjectCN: "ADV"}}
	app := newTestApp(NewHandler(stub))

	pfx := makePFX(t, "ADV", "pw", time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
	body, ct := multipartPFX(t, pfx, "pw")

	req := httptest.NewRequest(http.MethodPost, "/v1/certificates", body)
	req.Header.Set("Content-Type", ct)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusCreated, resp.StatusCode)
	// tenant_id + owner_user_id come from the principal, never the body.
	is.Equal("tenant-1", stub.gotCreate.TenantID)
	is.Equal("owner-1", stub.gotCreate.OwnerUserID)
	is.Equal("pw", stub.gotCreate.Password)
	is.True(bytes.Equal(pfx, stub.gotCreate.PFXData))

	var got CertificateView
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	is.Equal("c1", got.ID)
}

func TestHandler_Create_MissingPassword_400(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	stub := &stubUC{}
	app := newTestApp(NewHandler(stub))

	body, ct := multipartPFX(t, []byte("bytes"), "") // no password
	req := httptest.NewRequest(http.MethodPost, "/v1/certificates", body)
	req.Header.Set("Content-Type", ct)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusBadRequest, resp.StatusCode)
	is.Equal(CreateCommand{}, stub.gotCreate, "use case must not be called without a password")
}

func TestHandler_Create_MissingFile_400(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	app := newTestApp(NewHandler(&stubUC{}))
	body, ct := multipartPFX(t, nil, "pw") // no file
	req := httptest.NewRequest(http.MethodPost, "/v1/certificates", body)
	req.Header.Set("Content-Type", ct)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusBadRequest, resp.StatusCode)
}

func TestHandler_Preview_200(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	stub := &stubUC{previewRes: PreviewResult{
		SubjectCN: "MARIA", OAB: "12345/SP",
		Checks: PreviewChecks{NaoExpirado: true, TitularConfere: true, CadeiaOk: false},
	}}
	app := newTestApp(NewHandler(stub))

	body, ct := multipartPFX(t, []byte("pfx"), "pw")
	req := httptest.NewRequest(http.MethodPost, "/v1/certificates/preview", body)
	req.Header.Set("Content-Type", ct)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusOK, resp.StatusCode)
	is.Equal("pw", stub.gotPreview.Password)

	var got PreviewResult
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	is.Equal("MARIA", got.SubjectCN)
	is.True(got.Checks.NaoExpirado)
	is.False(got.Checks.CadeiaOk)
}

func TestHandler_Preview_WrongPassword_400(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	app := newTestApp(NewHandler(&stubUC{previewErr: ErrInvalidPassword}))
	body, ct := multipartPFX(t, []byte("pfx"), "wrong")
	req := httptest.NewRequest(http.MethodPost, "/v1/certificates/preview", body)
	req.Header.Set("Content-Type", ct)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusBadRequest, resp.StatusCode)
}

func TestHandler_List_200_Envelope(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	stub := &stubUC{listRes: []CertificateView{{ID: "c1"}, {ID: "c2"}}}
	app := newTestApp(NewHandler(stub))

	req := httptest.NewRequest(http.MethodGet, "/v1/certificates", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusOK, resp.StatusCode)
	var env certificatesEnvelope
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
	is.Len(env.Data, 2)
}

func TestHandler_List_Empty_SerializesEmptyArray(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	app := newTestApp(NewHandler(&stubUC{listRes: nil}))
	req := httptest.NewRequest(http.MethodGet, "/v1/certificates", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	is.Contains(string(raw), `"data":[]`, "empty list must serialize as [] not null")
}

func TestHandler_Revoke_204(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	stub := &stubUC{}
	app := newTestApp(NewHandler(stub))

	req := httptest.NewRequest(http.MethodDelete, "/v1/certificates/cert-9", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusNoContent, resp.StatusCode)
	is.Equal("tenant-1", stub.gotRevTID)
	is.Equal("cert-9", stub.gotRevID)
}

func TestHandler_Revoke_NotFound_404(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	app := newTestApp(NewHandler(&stubUC{revokeErr: ErrCertificateNotFound}))
	req := httptest.NewRequest(http.MethodDelete, "/v1/certificates/missing", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusNotFound, resp.StatusCode)
}
