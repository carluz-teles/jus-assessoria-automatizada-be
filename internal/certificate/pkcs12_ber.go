package certificate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"time"
)

// opensslTimeout bounds each openssl hop — a hung subprocess must never
// stall a certificate request indefinitely.
const opensslTimeout = 5 * time.Second

// normalizeBERToDER re-encodes a BER PKCS#12 as a fresh DER one by
// round-tripping it through openssl, whose ASN.1 decoder reads BER natively
// (unlike go-pkcs12/encoding/asn1). Two hops: extract key+chain as PEM using
// the real password (hop 1), then re-export as DER under a throwaway
// password (hop 2) that the caller re-parses normally with decodeDER.
// Passwords travel via OS pipes, never argv; the pfx/PEM bytes travel via
// unlinked temp files (see ephemeralFile) since openssl's export path needs
// a seekable input. Nothing here ever gets a real path on disk.
func normalizeBERToDER(ctx context.Context, pfx []byte, password string) ([]byte, string, error) {
	pem, err := opensslExtractPEM(ctx, pfx, password)
	if err != nil {
		return nil, "", err
	}
	newPassword, err := randomHex(16)
	if err != nil {
		return nil, "", ErrPKCS12Parse
	}
	der, err := opensslExportDER(ctx, pem, newPassword)
	if err != nil {
		return nil, "", ErrPKCS12Parse
	}
	return der, newPassword, nil
}

// opensslExtractPEM decrypts pfx with password and dumps its key + cert
// chain as PEM. pfx travels via stdin ("/dev/stdin" — "-in -" isn't
// recognized by every openssl pkcs12 code path); password via a dedicated
// pipe ("-passin fd:3") so it never appears in argv, visible to `ps`.
// "-legacy" loads OpenSSL 3.x's legacy provider: real-world old .pfx exports
// that trip the BER path (Windows certutil/certmgr, older tokens/HSMs) very
// often also encrypt the cert bag with RC2-40-CBC, which OpenSSL 3.0+
// no longer supports in its default provider — without this flag the hop
// fails with "unsupported algorithm" and the file is wrongly rejected as
// invalid even though the password was correct.
func opensslExtractPEM(ctx context.Context, pfx []byte, password string) ([]byte, error) {
	out, stderr, err := runOpenSSL(ctx, pfx, password, "pkcs12", "-in", "/dev/stdin", "-nodes", "-passin", "fd:3", "-legacy")
	if err != nil {
		return nil, classifyPKCS12Error(errWithStderr{stderr})
	}
	return out, nil
}

// opensslExportDER repackages a PEM bundle (cert(s) + unencrypted key, as
// produced by opensslExtractPEM) as a DER PKCS#12 protected by newPassword —
// again passed via a pipe, never argv.
func opensslExportDER(ctx context.Context, pemBundle []byte, newPassword string) ([]byte, error) {
	out, _, err := runOpenSSL(ctx, pemBundle, newPassword, "pkcs12", "-export", "-in", "/dev/stdin", "-passout", "fd:3")
	if err != nil {
		return nil, ErrPKCS12Parse
	}
	return out, nil
}

// errWithStderr adapts a raw stderr string to the error interface so
// classifyPKCS12Error (written for pkcs12.DecodeChain's Go errors) can reuse
// the same "password"/"mac verify" heuristic against openssl's stderr text —
// both report PKCS#12 MAC failures with that wording.
type errWithStderr struct{ text string }

func (e errWithStderr) Error() string { return e.text }

// runOpenSSL executes `openssl <args>` with stdin and a secret fed through
// fd 3 (the openssl -passin/-passout "fd:3" convention). stdin is a real
// (unlinked) file, not a pipe: openssl's "-export" path reads its input in
// two passes — once for the certificate(s), once for the key — which a pipe
// can't rewind for. The secret pipe is written concurrently to avoid
// deadlocking on openssl's own stdout/stderr buffering.
func runOpenSSL(ctx context.Context, stdin []byte, secret string, args ...string) (stdout []byte, stderr string, err error) {
	ctx, cancel := context.WithTimeout(ctx, opensslTimeout)
	defer cancel()

	stdinFile, err := ephemeralFile(stdin)
	if err != nil {
		return nil, "", err
	}
	defer stdinFile.Close()

	secretR, secretW, err := os.Pipe()
	if err != nil {
		return nil, "", err
	}
	defer secretR.Close()

	cmd := exec.CommandContext(ctx, "openssl", args...)
	cmd.Stdin = stdinFile
	cmd.ExtraFiles = []*os.File{secretR} // becomes fd 3 in the child
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	go func() {
		defer secretW.Close()
		// Best-effort: if openssl exits before reading (e.g. bad args), this
		// write errors too — the classification comes from cmd.Run()'s exit
		// code/stderr below, not from here.
		_, _ = secretW.Write([]byte(secret))
	}()

	if err := cmd.Run(); err != nil {
		return nil, stderrBuf.String(), err
	}
	return stdoutBuf.Bytes(), stderrBuf.String(), nil
}

// ephemeralFile writes data to a temp file, unlinks it immediately, and
// rewinds it — the returned *os.File is a valid, seekable fd with no
// directory entry anywhere (nothing to clean up, nothing another process can
// open by path). Cert/key material passed to openssl this way never persists
// on disk, only in this already-unlinked, memory-backed (on the container's
// tmpfs /tmp) inode.
func ephemeralFile(data []byte) (*os.File, error) {
	f, err := os.CreateTemp("", "pkcs12-*")
	if err != nil {
		return nil, err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// randomHex generates a throwaway password for the intermediate DER PKCS#12
// (hop 2 of normalizeBERToDER) — it never leaves this process, so it only
// needs to be unpredictable enough to not collide with anything, not
// memorable.
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
