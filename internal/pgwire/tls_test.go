package pgwire

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// makeCA returns a self-signed authority and its PEM.
func makeCA(t *testing.T, name string) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return cert, key, pemBytes
}

// makeLeaf returns a server certificate signed by the given authority.
func makeLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey) []byte {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "db.internal"},
		DNSNames:     []string{"db.internal"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func TestBuildTLSConfig(t *testing.T) {
	_, _, caPEM := makeCA(t, "test root")
	path := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(path, caPEM, 0o644)

	// verify-full with a custom root: hostname checked against the pool.
	cfg, err := buildTLSConfig(Config{
		Host: "db.internal", SSLMode: "verify-full", SSLRootCert: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ServerName != "db.internal" || cfg.RootCAs == nil || cfg.InsecureSkipVerify {
		t.Fatalf("verify-full cfg = %+v", cfg)
	}

	// require plus a root certificate is promoted to verify-ca.
	cfg, err = buildTLSConfig(Config{SSLMode: "require", SSLRootCert: path})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.InsecureSkipVerify || cfg.VerifyPeerCertificate == nil {
		t.Fatalf("require+rootcert cfg = %+v", cfg)
	}

	// Plain require stays encrypt-only.
	cfg, _ = buildTLSConfig(Config{SSLMode: "require"})
	if cfg.VerifyPeerCertificate != nil || !cfg.InsecureSkipVerify {
		t.Fatalf("require cfg = %+v", cfg)
	}

	// A missing or empty PEM file is a loud error.
	if _, err := buildTLSConfig(Config{SSLMode: "verify-full",
		SSLRootCert: filepath.Join(t.TempDir(), "absent.pem")}); err == nil {
		t.Error("want error for a missing sslrootcert")
	}
	empty := filepath.Join(t.TempDir(), "empty.pem")
	os.WriteFile(empty, []byte("not a pem"), 0o644)
	if _, err := buildTLSConfig(Config{SSLMode: "verify-full",
		SSLRootCert: empty}); err == nil {
		t.Error("want error for a certless sslrootcert")
	}
}

func TestChainVerifier(t *testing.T) {
	ca, caKey, caPEM := makeCA(t, "good root")
	leaf := makeLeaf(t, ca, caKey)

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	verify := chainVerifier(pool)

	if err := verify([][]byte{leaf}, nil); err != nil {
		t.Fatalf("cert signed by the trusted root must pass: %v", err)
	}

	// A certificate from a different authority must fail.
	otherCA, otherKey, _ := makeCA(t, "other root")
	stranger := makeLeaf(t, otherCA, otherKey)
	if err := verify([][]byte{stranger}, nil); err == nil {
		t.Fatal("cert from an untrusted root must fail")
	}

	if err := verify(nil, nil); err == nil {
		t.Fatal("no certificate must fail")
	}
}

func TestParseURLSSLRootCert(t *testing.T) {
	cfg, err := ParseURL(
		"postgres://u:p@db.example.com/app?sslmode=verify-ca&sslrootcert=/etc/ssl/ca.pem")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SSLMode != "verify-ca" || cfg.SSLRootCert != "/etc/ssl/ca.pem" {
		t.Fatalf("cfg = %+v", cfg)
	}
}
