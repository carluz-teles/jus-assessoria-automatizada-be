package certificate

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jusassessoria/platform/lib/httpx"
)

// newTestApp mounts the certificate routes behind a middleware that injects a fixed
// principal (standing in for the resolved Clerk auth), so the handler is tested with
// a real UseCase wired to fakes (repo/cipher/outbox), isolated from the auth chain
// and from Postgres/KMS.
func newTestApp(uc *UseCase) *fiber.App {
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		httpx.SetPrincipal(c, httpx.Principal{UserID: "owner-1", TenantID: "tenant-1", Role: "LAWYER"})
		return c.Next()
	})
	New(uc).RegisterV1(app.Group("/v1"))
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

func TestHandler_Upload_201(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	repo := &fakeRepo{}
	uc := NewUseCase(repo, &fakeUOW{}, newFakeCipher(t), &fakeOutbox{})
	app := newTestApp(uc)

	pfx := generateTestPFX(t, "ADV", "pw", time.Hour)
	body, ct := multipartPFX(t, pfx, "pw")

	req := httptest.NewRequest(http.MethodPost, "/v1/certificates", body)
	req.Header.Set("Content-Type", ct)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusCreated, resp.StatusCode)
	// tenant_id + owner_user_id come from the principal, never the body.
	is.Equal("tenant-1", repo.inserted.TenantID)
	is.Equal("owner-1", repo.inserted.OwnerUserID)

	var got certificateView
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	is.Equal("ADV", got.SubjectCN)
}

func TestHandler_Upload_MissingPassword_400(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	repo := &fakeRepo{}
	app := newTestApp(NewUseCase(repo, &fakeUOW{}, newFakeCipher(t), &fakeOutbox{}))

	body, ct := multipartPFX(t, []byte("bytes"), "") // no password
	req := httptest.NewRequest(http.MethodPost, "/v1/certificates", body)
	req.Header.Set("Content-Type", ct)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusBadRequest, resp.StatusCode)
	is.Nil(repo.inserted, "use case must not be called without a password")
}

func TestHandler_Upload_MissingFile_400(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	app := newTestApp(NewUseCase(&fakeRepo{}, &fakeUOW{}, newFakeCipher(t), &fakeOutbox{}))
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

	app := newTestApp(NewUseCase(&fakeRepo{}, &fakeUOW{}, newFakeCipher(t), &fakeOutbox{}))

	pfx := generateTestPFX(t, "MARIA", "pw", time.Hour)
	body, ct := multipartPFX(t, pfx, "pw")
	req := httptest.NewRequest(http.MethodPost, "/v1/certificates/preview", body)
	req.Header.Set("Content-Type", ct)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusOK, resp.StatusCode)

	var got previewResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	is.Equal("MARIA", got.SubjectCN)
	is.True(got.Checks.NaoExpirado)
	is.False(got.Checks.CadeiaOk)
}

func TestHandler_Preview_WrongPassword_400(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	app := newTestApp(NewUseCase(&fakeRepo{}, &fakeUOW{}, newFakeCipher(t), &fakeOutbox{}))
	pfx := generateTestPFX(t, "CN", "right", time.Hour)
	body, ct := multipartPFX(t, pfx, "wrong")
	req := httptest.NewRequest(http.MethodPost, "/v1/certificates/preview", body)
	req.Header.Set("Content-Type", ct)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusBadRequest, resp.StatusCode)
}

func TestHandler_Sign_200_RecordsAuditWithPrincipalAsSigner(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	repo := &fakeRepo{}
	uc := NewUseCase(repo, &fakeUOW{}, newFakeCipher(t), &fakeOutbox{})
	cert := sealedCertificate(t, uc, repo, "ADV SIGNER", "pw", time.Hour)
	repo.getRes = cert
	app := newTestApp(uc)

	sum := sha256.Sum256([]byte("doc"))
	digestB64 := base64.StdEncoding.EncodeToString(sum[:])
	body := `{"password":"pw","digest_sha256":"` + digestB64 + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/certificates/cert-1/sign", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusOK, resp.StatusCode)
	// The signer of the audit row comes from the principal, never the body.
	is.Equal("owner-1", repo.recordedUser)
	is.Equal("tenant-1", repo.recordedTID)

	var got map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	is.NotEmpty(got["signature"])
	is.NotEmpty(got["cert_chain"])
}

// TestHandler_Sign_MissingPassword_400 pins the fix for the security bug: a
// certificate whose PasswordPolicy requires a password (the default, "always")
// must reject a request that omits it. Password is no longer
// validation.Required on SignRequest's shape — the 400 now comes from the
// domain (ErrInvalidPassword), which is why the cert must resolve via
// repo.getRes instead of short-circuiting at request validation.
func TestHandler_Sign_MissingPassword_400(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	repo := &fakeRepo{}
	uc := NewUseCase(repo, &fakeUOW{}, newFakeCipher(t), &fakeOutbox{})
	cert := sealedCertificate(t, uc, repo, "ADV", "pw", time.Hour)
	repo.getRes = cert
	app := newTestApp(uc)

	sum := sha256.Sum256([]byte("doc"))
	body := `{"digest_sha256":"` + base64.StdEncoding.EncodeToString(sum[:]) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/certificates/cert-1/sign", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusBadRequest, resp.StatusCode)
	is.Nil(repo.recordedDig, "signing must never proceed without a matching password")
}

// TestHandler_Sign_WrongPassword_400 pins the core bug: before the fix, Sign()
// discarded the password parameter (`_ string`) and any value — including a
// wrong one — signed successfully.
func TestHandler_Sign_WrongPassword_400(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	repo := &fakeRepo{}
	uc := NewUseCase(repo, &fakeUOW{}, newFakeCipher(t), &fakeOutbox{})
	cert := sealedCertificate(t, uc, repo, "ADV", "right-pw", time.Hour)
	repo.getRes = cert
	app := newTestApp(uc)

	sum := sha256.Sum256([]byte("doc"))
	body := `{"password":"wrong-pw","digest_sha256":"` + base64.StdEncoding.EncodeToString(sum[:]) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/certificates/cert-1/sign", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusBadRequest, resp.StatusCode)
	is.Nil(repo.recordedDig, "signing must never proceed with a wrong password")
}

func TestHandler_Sign_NotFound_404(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	repo := &fakeRepo{getErr: ErrCertificateNotFound}
	app := newTestApp(NewUseCase(repo, &fakeUOW{}, newFakeCipher(t), &fakeOutbox{}))
	sum := sha256.Sum256([]byte("doc"))
	body := `{"password":"pw","digest_sha256":"` + base64.StdEncoding.EncodeToString(sum[:]) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/certificates/missing/sign", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusNotFound, resp.StatusCode)
}

func TestHandler_List_200(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	repo := &fakeRepo{listViews: []CertificateWithOwner{
		{Certificate: Certificate{ID: "c1"}},
		{Certificate: Certificate{ID: "c2"}},
	}}
	app := newTestApp(NewUseCase(repo, &fakeUOW{}, newFakeCipher(t), &fakeOutbox{}))

	req := httptest.NewRequest(http.MethodGet, "/v1/certificates", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusOK, resp.StatusCode)
	var env struct {
		Data []certificateView `json:"data"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
	is.Len(env.Data, 2)
}

func TestHandler_List_Empty_SerializesEmptyArray(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	app := newTestApp(NewUseCase(&fakeRepo{listViews: nil}, &fakeUOW{}, newFakeCipher(t), &fakeOutbox{}))
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

	repo := &fakeRepo{}
	app := newTestApp(NewUseCase(repo, &fakeUOW{}, newFakeCipher(t), &fakeOutbox{}))

	req := httptest.NewRequest(http.MethodDelete, "/v1/certificates/cert-9", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusNoContent, resp.StatusCode)
	is.Equal("tenant-1", repo.revokedTID)
	is.Equal("cert-9", repo.revokedID)
}

func TestHandler_UpdatePasswordPolicy_200(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	updated := &Certificate{ID: "cert-1", PasswordPolicy: PasswordPolicyNever}
	repo := &fakeRepo{updateRes: updated}
	app := newTestApp(NewUseCase(repo, &fakeUOW{}, newFakeCipher(t), &fakeOutbox{}))

	body := `{"password_policy":"never"}`
	req := httptest.NewRequest(http.MethodPatch, "/v1/certificates/cert-1/password-policy", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusOK, resp.StatusCode)
	is.Equal(PasswordPolicyNever, repo.updatedPolicy)
	is.Equal("tenant-1", repo.updatedTID)

	var got certificateView
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	is.Equal("never", got.PasswordPolicy)
}

func TestHandler_UpdatePasswordPolicy_InvalidValue_400(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	repo := &fakeRepo{}
	app := newTestApp(NewUseCase(repo, &fakeUOW{}, newFakeCipher(t), &fakeOutbox{}))

	body := `{"password_policy":"bogus"}`
	req := httptest.NewRequest(http.MethodPatch, "/v1/certificates/cert-1/password-policy", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusBadRequest, resp.StatusCode)
	is.Zero(repo.updatePolicyCalls)
}

func TestHandler_UpdatePasswordPolicy_MissingField_400(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	repo := &fakeRepo{}
	app := newTestApp(NewUseCase(repo, &fakeUOW{}, newFakeCipher(t), &fakeOutbox{}))

	req := httptest.NewRequest(http.MethodPatch, "/v1/certificates/cert-1/password-policy", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusBadRequest, resp.StatusCode)
	is.Zero(repo.updatePolicyCalls)
}

func TestHandler_UpdatePasswordPolicy_NotFound_404(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	repo := &fakeRepo{updateErr: ErrCertificateNotFound}
	app := newTestApp(NewUseCase(repo, &fakeUOW{}, newFakeCipher(t), &fakeOutbox{}))

	body := `{"password_policy":"always"}`
	req := httptest.NewRequest(http.MethodPatch, "/v1/certificates/missing/password-policy", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusNotFound, resp.StatusCode)
}

func TestHandler_Revoke_NotFound_404(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	app := newTestApp(NewUseCase(&fakeRepo{revokeErr: ErrCertificateNotFound}, &fakeUOW{}, newFakeCipher(t), &fakeOutbox{}))
	req := httptest.NewRequest(http.MethodDelete, "/v1/certificates/missing", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusNotFound, resp.StatusCode)
}
