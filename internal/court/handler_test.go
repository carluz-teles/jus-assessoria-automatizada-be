package court

import (
	"bytes"
	"encoding/json"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/makiuchi-d/gozxing"
	"github.com/makiuchi-d/gozxing/qrcode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jusassessoria/platform/lib/httpx"
)

// newTestApp mounts the court routes behind a middleware that injects a fixed
// principal (standing in for the resolved Clerk auth), isolated from the auth chain
// and from Postgres/vault (the UseCase is wired to fakes/a real in-memory vault).
func newTestApp(uc *UseCase) *fiber.App {
	app := fiber.New()
	app.Use(func(c *fiber.Ctx) error {
		httpx.SetPrincipal(c, httpx.Principal{UserID: "user-1", TenantID: "tenant-1", Role: "LAWYER"})
		return c.Next()
	})
	New(uc).RegisterV1(app.Group("/v1"))
	return app
}

func multipartSecret(t *testing.T, secret string) (body *bytes.Buffer, contentType string) {
	t.Helper()
	body = &bytes.Buffer{}
	w := multipart.NewWriter(body)
	require.NoError(t, w.WriteField("secret", secret))
	require.NoError(t, w.Close())
	return body, w.FormDataContentType()
}

func multipartQR(t *testing.T, otpauthURI string) (body *bytes.Buffer, contentType string) {
	t.Helper()
	matrix, err := qrcode.NewQRCodeWriter().EncodeWithoutHint(otpauthURI, gozxing.BarcodeFormat_QR_CODE, 256, 256)
	require.NoError(t, err)

	body = &bytes.Buffer{}
	w := multipart.NewWriter(body)
	fw, err := w.CreateFormFile("qr", "totp.png")
	require.NoError(t, err)
	require.NoError(t, png.Encode(fw, matrix))
	require.NoError(t, w.Close())
	return body, w.FormDataContentType()
}

func TestHandler_SubmitMFASeed_ViaSecretText_200(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	repo := newFakeRepo(newConn())
	provider := &fakeProvider{}
	uc := NewUseCase(repo, fakeUOW{}, testVault(t), &fakeOutbox{})
	uc.RegisterProvider("EPROC", provider)
	app := newTestApp(uc)

	body, ct := multipartSecret(t, "JBSWY3DPEHPK3PXP")
	req := httptest.NewRequest(http.MethodPost, "/v1/court-connections/conn-1/mfa-seed", body)
	req.Header.Set("Content-Type", ct)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusOK, resp.StatusCode)
	var got connectionView
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	is.Equal(string(StatusConnected), got.Status)
	is.Equal("JBSWY3DPEHPK3PXP", provider.connectCalls[0])
}

func TestHandler_SubmitMFASeed_ViaQRScreenshot_200(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	repo := newFakeRepo(newConn())
	provider := &fakeProvider{}
	uc := NewUseCase(repo, fakeUOW{}, testVault(t), &fakeOutbox{})
	uc.RegisterProvider("EPROC", provider)
	app := newTestApp(uc)

	body, ct := multipartQR(t, "otpauth://totp/eproc:luan.gomes?secret=JBSWY3DPEHPK3PXP&issuer=eproc")
	req := httptest.NewRequest(http.MethodPost, "/v1/court-connections/conn-1/mfa-seed", body)
	req.Header.Set("Content-Type", ct)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusOK, resp.StatusCode)
	var got connectionView
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	is.Equal(string(StatusConnected), got.Status)
	is.Equal("JBSWY3DPEHPK3PXP", provider.connectCalls[0], "secret extracted from the decoded QR payload")
}

func TestHandler_SubmitMFASeed_MissingBoth_400(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	repo := newFakeRepo(newConn())
	uc := NewUseCase(repo, fakeUOW{}, testVault(t), &fakeOutbox{})
	uc.RegisterProvider("EPROC", &fakeProvider{})
	app := newTestApp(uc)

	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)
	require.NoError(t, w.Close())

	req := httptest.NewRequest(http.MethodPost, "/v1/court-connections/conn-1/mfa-seed", body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusBadRequest, resp.StatusCode)
}

func TestHandler_SubmitMFASeed_ProviderRejects_200WithErrorStatus(t *testing.T) {
	// Mirrors h.connect's contract: a provider-side rejection is informative state
	// (200 + ERROR status), not a 500 — the FE needs to render "still not connected",
	// not treat it as an unexpected failure.
	t.Parallel()
	is := assert.New(t)

	repo := newFakeRepo(newConn())
	provider := &fakeProvider{connectErrs: []error{assert.AnError}}
	uc := NewUseCase(repo, fakeUOW{}, testVault(t), &fakeOutbox{})
	uc.RegisterProvider("EPROC", provider)
	app := newTestApp(uc)

	body, ct := multipartSecret(t, "WRONGSEED")
	req := httptest.NewRequest(http.MethodPost, "/v1/court-connections/conn-1/mfa-seed", body)
	req.Header.Set("Content-Type", ct)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusOK, resp.StatusCode)
	var got connectionView
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	is.Equal(string(StatusError), got.Status)
}
