package extproc

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestServerOptionsValidate(t *testing.T) {
	t.Parallel()

	defaults := NewDefaultServerOptions()
	if err := defaults.Validate(); err != nil {
		t.Fatalf("default options must be valid: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*ServerOptions)
	}{
		{"zero port", func(o *ServerOptions) { o.ExtProcPort = 0 }},
		{"same port", func(o *ServerOptions) { o.MetricsHTTPPort = o.ExtProcPort }},
		{"zero streams", func(o *ServerOptions) { o.MaxConcurrentStreams = 0 }},
		{"zero recv size", func(o *ServerOptions) { o.MaxRecvMsgSize = 0 }},
		{"zero send size", func(o *ServerOptions) { o.MaxSendMsgSize = 0 }},
		{"negative grace", func(o *ServerOptions) { o.TerminationGracePeriodSeconds = -1 }},
		{"tls without cert", func(o *ServerOptions) { o.TLS = &TLSOptions{KeyFile: "k"} }},
		{"invalid tls version", func(o *ServerOptions) { o.TLS = &TLSOptions{CertFile: "c", KeyFile: "k", MinVersion: "1.1"} }},
		{"negative tls reload", func(o *ServerOptions) { o.TLS = &TLSOptions{CertFile: "c", KeyFile: "k", ReloadIntervalSeconds: -1} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := NewDefaultServerOptions()
			tt.mutate(opts)
			if err := opts.Validate(); err == nil {
				t.Fatal("expected validation to fail")
			}
		})
	}
}

func TestIsLoopbackBind(t *testing.T) {
	t.Parallel()

	tests := map[string]bool{
		"":          false,
		"0.0.0.0":   false,
		"::":        false,
		"10.0.0.1":  false,
		"127.0.0.1": true,
		"::1":       true,
		"localhost": true,
	}
	for addr, want := range tests {
		if got := isLoopbackBind(addr); got != want {
			t.Fatalf("isLoopbackBind(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestLoadTLSConfig(t *testing.T) {
	t.Parallel()

	certFile, keyFile := writeSelfSignedCert(t, "server")
	cfg, err := loadTLSConfig(&TLSOptions{CertFile: certFile, KeyFile: keyFile, MinVersion: "1.3"})
	if err != nil {
		t.Fatalf("loadTLSConfig failed: %v", err)
	}
	if cfg == nil || len(cfg.Certificates) != 1 {
		t.Fatal("expected one server certificate")
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Fatalf("min version mismatch: %d", cfg.MinVersion)
	}
	if cfg.ClientAuth != tls.NoClientCert {
		t.Fatal("mTLS must only be enabled when a client CA is configured")
	}

	mtls, err := loadTLSConfig(&TLSOptions{CertFile: certFile, KeyFile: keyFile, ClientCAFile: certFile})
	if err != nil {
		t.Fatalf("mTLS loadTLSConfig failed: %v", err)
	}
	if mtls.ClientAuth != tls.RequireAndVerifyClientCert || mtls.ClientCAs == nil {
		t.Fatal("clientCAFile must enable verified mutual TLS")
	}

	if _, err := loadTLSConfig(&TLSOptions{CertFile: filepath.Join(t.TempDir(), "missing"), KeyFile: keyFile}); err == nil {
		t.Fatal("a missing certificate must fail")
	}
}

// writeSelfSignedCert generates a throwaway server certificate for tests.
func writeSelfSignedCert(t *testing.T, name string) (certFile, keyFile string) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certFile, keyFile
}

func TestTLSReloadSwapsAndKeepsOldOnFailure(t *testing.T) {
	t.Parallel()

	certA, keyA := writeSelfSignedCert(t, "server-a")
	certB, keyB := writeSelfSignedCert(t, "server-b")
	opts := &TLSOptions{CertFile: certA, KeyFile: keyA, MinVersion: "1.2"}
	r, err := newTLSReloader(opts, testSdkLogger)
	if err != nil {
		t.Fatalf("newTLSReloader failed: %v", err)
	}
	defer r.close()

	before := append([]byte(nil), r.cfg.Certificates[0].Certificate[0]...)
	certPEM, err := os.ReadFile(certB)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := os.ReadFile(keyB)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certA, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyA, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.reload(); err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	after := append([]byte(nil), r.cfg.Certificates[0].Certificate[0]...)
	if bytes.Equal(before, after) {
		t.Fatal("certificate reload did not swap the material")
	}
	sameCfg := r.cfg
	if err := r.reload(); err != nil {
		t.Fatalf("unchanged reload failed: %v", err)
	}
	if r.cfg != sameCfg {
		t.Fatal("an unchanged certificate must not be swapped or logged")
	}

	// A damaged file must not replace the last known-good material.
	if err := os.WriteFile(certA, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.reload(); err == nil {
		t.Fatal("reload of an invalid certificate must fail")
	}
	if !bytes.Equal(after, r.cfg.Certificates[0].Certificate[0]) {
		t.Fatal("a failed reload must keep the previous certificate")
	}
}
