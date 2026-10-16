package proxmox_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mattoddie/proxmox-mcp/internal/config"
	"github.com/mattoddie/proxmox-mcp/internal/js"
	"github.com/mattoddie/proxmox-mcp/internal/proxmox"
)

// selfSigned builds a throwaway self-signed certificate for 127.0.0.1/localhost in memory, so
// the TLS tests need neither openssl nor a private key checked into the repo.
func selfSigned(t *testing.T) (tls.Certificate, []byte, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(0x01234567),
		Subject:               pkix.Name{CommonName: "proxmox-mcp test"},
		NotBefore:             time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:              time.Date(2049, 12, 31, 23, 59, 59, 0, time.UTC),
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pemCert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pemCert, proxmox.Fingerprint(der)
}

func TestFingerprintParsing(t *testing.T) {
	colon := strings.Repeat("AB:", 31) + "CD"
	t.Run("accepts colon-separated, plain hex and prefixed fingerprints, normalised to AA:BB form", func(t *testing.T) {
		for in, want := range map[string]string{
			strings.ToLower(colon):                          colon,
			strings.ReplaceAll(colon, ":", ""):              colon,
			"sha256 Fingerprint=" + colon:                   colon,
			"SHA-256:" + colon:                              colon,
			colon + ", " + strings.Repeat("12", 32):         colon + "," + strings.Repeat("12:", 31) + "12",
			" " + strings.ReplaceAll(colon, ":", " ") + " ": colon,
		} {
			got, err := config.ParseFingerprints(in)
			if err != nil || strings.Join(got, ",") != want {
				t.Fatalf("%q: %v %v", in, got, err)
			}
		}
	})

	t.Run("rejects anything that isn't a SHA-256 fingerprint", func(t *testing.T) {
		for _, in := range []string{"AB:CD", strings.Repeat("zz", 32)} {
			if _, err := config.ParseFingerprints(in); err == nil || !strings.Contains(err.Error(), "must be a SHA-256 fingerprint") {
				t.Fatalf("%q: %v", in, err)
			}
		}
		_, err := config.Load(config.MapEnv(with(tokenEnv, "PROXMOX_URL", "http://pve:8006", "PROXMOX_TLS_FINGERPRINT", colon)))
		if err == nil || err.Error() != "PROXMOX_TLS_FINGERPRINT needs an https:// PROXMOX_URL" {
			t.Fatal(err)
		}
		cfg, err := config.Load(config.MapEnv(with(tokenEnv, "PROXMOX_URL", "https://pve:8006", "PROXMOX_TLS_FINGERPRINT", colon)))
		if err != nil || strings.Join(cfg.Proxmox.TLSFingerprints, ",") != colon {
			t.Fatal(cfg, err)
		}
	})
}

func TestTLSPinning(t *testing.T) {
	cert, pemCert, fingerprint := selfSigned(t)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"version":"9.0.0"}}`))
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	server.StartTLS()
	defer server.Close()
	ctx := context.Background()
	get := func(env ...string) (any, error) {
		c := newClient(t, with(tokenEnv, append([]string{"PROXMOX_URL", server.URL, "PROXMOX_TIMEOUT_MS", "3000"}, env...)...))
		return c.Get(ctx, "/version", nil)
	}
	wantVersion := func(v any, err error) {
		t.Helper()
		if err != nil || js.Stringify(v, "") != `{"version":"9.0.0"}` {
			t.Fatal(js.Stringify(v, ""), err)
		}
	}

	t.Run("connects when the certificate matches the pinned fingerprint", func(t *testing.T) {
		wantVersion(get("PROXMOX_TLS_FINGERPRINT", fingerprint))
		// Also as lower-case hex without colons, and alongside a second (e.g. renewed) certificate's fingerprint.
		hex := strings.ToLower(strings.ReplaceAll(fingerprint, ":", ""))
		wantVersion(get("PROXMOX_TLS_FINGERPRINT", strings.Repeat("00", 32)+","+hex))
		// With PROXMOX_VERIFY_SSL=true the chain is checked as well as the pin, so a self-signed certificate is still refused.
		_, err := get("PROXMOX_TLS_FINGERPRINT", hex, "PROXMOX_VERIFY_SSL", "true")
		mustMatch(t, err, `Could not reach Proxmox`)
	})

	t.Run("refuses a certificate that doesn't match, even with verification off", func(t *testing.T) {
		_, err := get("PROXMOX_TLS_FINGERPRINT", strings.Repeat("AB:", 31)+"CD")
		mustMatch(t, err, `^Could not reach Proxmox at https://127\.0\.0\.1:\d+: TLS certificate fingerprint mismatch: the server presented SHA-256 `+fingerprint+`, which is not in PROXMOX_TLS_FINGERPRINT \(Proxmox uses a self-signed certificate`)
		// Not pinned, verification off: accepted (the startup warning covers this case).
		wantVersion(get())
		// Not pinned, verification on: the self-signed certificate is refused with a hint about pinning.
		_, err = get("PROXMOX_VERIFY_SSL", "true")
		mustMatch(t, err, `DEPTH_ZERO_SELF_SIGNED_CERT \(Proxmox uses a self-signed certificate by default; pin it with PROXMOX_TLS_FINGERPRINT`)
	})

	t.Run("trusts PROXMOX_CA_FILE, which turns verification on", func(t *testing.T) {
		dir := t.TempDir()
		caFile := filepath.Join(dir, "ca.pem")
		if err := os.WriteFile(caFile, pemCert, 0o600); err != nil {
			t.Fatal(err)
		}
		wantVersion(get("PROXMOX_CA_FILE", caFile))
		cfg, _ := config.Load(config.MapEnv(with(tokenEnv, "PROXMOX_URL", server.URL, "PROXMOX_CA_FILE", caFile)))
		if !cfg.Proxmox.VerifySSL {
			t.Fatal("a CA file turns verification on")
		}
		// The CA plus a wrong pin: the pin still applies.
		_, err := get("PROXMOX_CA_FILE", caFile, "PROXMOX_TLS_FINGERPRINT", strings.Repeat("00", 32))
		mustMatch(t, err, `fingerprint mismatch`)
		_, err = config.Load(config.MapEnv(with(tokenEnv, "PROXMOX_URL", server.URL, "PROXMOX_CA_FILE", filepath.Join(dir, "missing.pem"))))
		if err == nil || !strings.HasPrefix(err.Error(), "Could not read PROXMOX_CA_FILE (") {
			t.Fatal(err)
		}
	})
}
