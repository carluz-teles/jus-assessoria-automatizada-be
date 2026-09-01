package court

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	return multipartQRWithFields(t, otpauthURI, nil)
}

// multipartQRWithFields is multipartQR extended with extra text form fields
// (e.g. "account_index") in the SAME multipart body — needed for the
// resubmit-with-selection flow, which sends the QR and the choice together.
func multipartQRWithFields(t *testing.T, otpauthURI string, fields map[string]string) (body *bytes.Buffer, contentType string) {
	t.Helper()
	matrix, err := qrcode.NewQRCodeWriter().EncodeWithoutHint(otpauthURI, gozxing.BarcodeFormat_QR_CODE, 256, 256)
	require.NoError(t, err)

	body = &bytes.Buffer{}
	w := multipart.NewWriter(body)
	fw, err := w.CreateFormFile("qr", "totp.png")
	require.NoError(t, err)
	require.NoError(t, png.Encode(fw, matrix))
	for k, v := range fields {
		require.NoError(t, w.WriteField(k, v))
	}
	require.NoError(t, w.Close())
	return body, w.FormDataContentType()
}

// migrationAccount is this file's minimal input shape for building a Google
// Authenticator "Export accounts" migration QR — always TOTP/SHA1/6-digit
// (the wire format itself is exhaustively covered by lib/totp's own tests, not
// re-verified here; these tests only exercise the HANDLER's multi-candidate
// dispatch).
type migrationAccount struct{ issuer, name, secret string }

// migrationURI builds a real "otpauth-migration://offline?data=..." URI from
// a list of accounts.
func migrationURI(accounts []migrationAccount) string {
	var payload []byte
	for _, a := range accounts {
		var param []byte
		param = appendLenDelimField(param, 1, []byte(a.secret))
		param = appendLenDelimField(param, 2, []byte(a.name))
		param = appendLenDelimField(param, 3, []byte(a.issuer))
		param = appendVarintField(param, 6, 2) // OtpType.TOTP
		payload = appendLenDelimField(payload, 1, param)
	}
	data := base64.StdEncoding.EncodeToString(payload)
	return "otpauth-migration://offline?data=" + url.QueryEscape(data)
}

func multipartMigrationQR(t *testing.T, accounts []migrationAccount) (body *bytes.Buffer, contentType string) {
	t.Helper()
	return multipartQR(t, migrationURI(accounts))
}

func appendVarint(buf []byte, v uint64) []byte {
	for v >= 0x80 {
		buf = append(buf, byte(v)|0x80)
		v >>= 7
	}
	return append(buf, byte(v))
}

func appendVarintField(buf []byte, fieldNum int, v uint64) []byte {
	buf = appendVarint(buf, uint64(fieldNum)<<3|0)
	return appendVarint(buf, v)
}

func appendLenDelimField(buf []byte, fieldNum int, payload []byte) []byte {
	buf = appendVarint(buf, uint64(fieldNum)<<3|2)
	buf = appendVarint(buf, uint64(len(payload)))
	return append(buf, payload...)
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

// TestHandler_SubmitMFASeed_ViaMigrationQR_SingleAccount_200 proves a Google
// Authenticator export QR with exactly ONE account behaves identically to the
// original single-secret flow — no picker, straight to Connect.
func TestHandler_SubmitMFASeed_ViaMigrationQR_SingleAccount_200(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	repo := newFakeRepo(newConn())
	provider := &fakeProvider{}
	uc := NewUseCase(repo, fakeUOW{}, testVault(t), &fakeOutbox{})
	uc.RegisterProvider("EPROC", provider)
	app := newTestApp(uc)

	body, ct := multipartMigrationQR(t, []migrationAccount{
		{issuer: "eproc", name: "luan.gomes", secret: "xxxxxxxxxxxxxxxxxxxx"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/court-connections/conn-1/mfa-seed", body)
	req.Header.Set("Content-Type", ct)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusOK, resp.StatusCode)
	var got connectionView
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	is.Equal(string(StatusConnected), got.Status)
	require.Len(t, provider.connectCalls, 1)
}

// TestHandler_SubmitMFASeed_ViaMigrationQR_MultipleAccounts_NeedsSelection
// proves an export QR bundling SEVERAL accounts does NOT guess: it responds
// with labels only (no secrets) and never calls the provider.
func TestHandler_SubmitMFASeed_ViaMigrationQR_MultipleAccounts_NeedsSelection(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	repo := newFakeRepo(newConn())
	provider := &fakeProvider{}
	uc := NewUseCase(repo, fakeUOW{}, testVault(t), &fakeOutbox{})
	uc.RegisterProvider("EPROC", provider)
	app := newTestApp(uc)

	body, ct := multipartMigrationQR(t, []migrationAccount{
		{issuer: "eproc", name: "luan.gomes", secret: "secret-one-xxxxxxxxx"},
		{issuer: "GitHub", name: "luan@github", secret: "secret-two-xxxxxxxxx"},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/court-connections/conn-1/mfa-seed", body)
	req.Header.Set("Content-Type", ct)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusOK, resp.StatusCode)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var got needsSelectionView
	require.NoError(t, json.Unmarshal(raw, &got))
	is.True(got.NeedsSelection)
	require.Len(t, got.Candidates, 2)
	is.Equal("eproc (luan.gomes)", got.Candidates[0].Label)
	is.Equal("GitHub (luan@github)", got.Candidates[1].Label)
	is.Empty(provider.connectCalls, "must not connect until the lawyer picks an account")

	is.NotContains(string(raw), "secret-one-xxxxxxxxx", "response must never carry secret material")
	is.NotContains(string(raw), "secret-two-xxxxxxxxx", "response must never carry secret material")
}

// TestHandler_SubmitMFASeed_ViaMigrationQR_WithAccountIndex_200 proves the
// resubmit-with-selection path: same QR, plus "account_index", proceeds with
// exactly that account's secret.
func TestHandler_SubmitMFASeed_ViaMigrationQR_WithAccountIndex_200(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	repo := newFakeRepo(newConn())
	provider := &fakeProvider{}
	uc := NewUseCase(repo, fakeUOW{}, testVault(t), &fakeOutbox{})
	uc.RegisterProvider("EPROC", provider)
	app := newTestApp(uc)

	accounts := []migrationAccount{
		{issuer: "eproc", name: "luan.gomes", secret: "secret-one-xxxxxxxxx"},
		{issuer: "GitHub", name: "luan@github", secret: "secret-two-xxxxxxxxx"},
	}
	body, ct := multipartQRWithFields(t, migrationURI(accounts), map[string]string{"account_index": "0"})

	req := httptest.NewRequest(http.MethodPost, "/v1/court-connections/conn-1/mfa-seed", body)
	req.Header.Set("Content-Type", ct)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusOK, resp.StatusCode)
	var got connectionView
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	is.Equal(string(StatusConnected), got.Status)
	require.Len(t, provider.connectCalls, 1)
}

func TestHandler_SubmitMFASeed_ViaMigrationQR_InvalidAccountIndex_400(t *testing.T) {
	t.Parallel()
	is := assert.New(t)

	repo := newFakeRepo(newConn())
	provider := &fakeProvider{}
	uc := NewUseCase(repo, fakeUOW{}, testVault(t), &fakeOutbox{})
	uc.RegisterProvider("EPROC", provider)
	app := newTestApp(uc)

	accounts := []migrationAccount{
		{issuer: "eproc", name: "luan.gomes", secret: "secret-one-xxxxxxxxx"},
		{issuer: "GitHub", name: "luan@github", secret: "secret-two-xxxxxxxxx"},
	}
	body, ct := multipartQRWithFields(t, migrationURI(accounts), map[string]string{"account_index": "7"})

	req := httptest.NewRequest(http.MethodPost, "/v1/court-connections/conn-1/mfa-seed", body)
	req.Header.Set("Content-Type", ct)
	resp, err := app.Test(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	is.Equal(fiber.StatusBadRequest, resp.StatusCode)
	is.Empty(provider.connectCalls)
}
