package testutil

// Shared in-memory certificate generation for TLS-pinning test suites. No
// openssl dependency — everything is crypto/x509 + crypto/ecdsa, so callers
// stay hermetic (the default tier has no external deps, per CLAUDE.md's
// Testing section).

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCA is an in-memory CA used to sign leaf certificates for httptest TLS
// servers.
type TestCA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
	PEM  []byte // PEM-encoded certificate, suitable for an endpoint's caFile
}

func NewTestCA(t *testing.T) *TestCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "relayLLM test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA cert: %v", err)
	}
	return &TestCA{
		Cert: cert,
		Key:  key,
		PEM:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}
}

// WriteCAFile persists the CA's PEM bundle to a temp file and returns its
// path, ready to use as a config.OpenAIEndpoint.CAFile.
func (ca *TestCA) WriteCAFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, ca.PEM, 0o600); err != nil {
		t.Fatalf("write ca file: %v", err)
	}
	return path
}

// TestLeaf bundles a leaf certificate in the shapes different callers need:
// tls.Certificate for httptest's srv.TLS, the parsed *x509.Certificate for
// fingerprinting, and PEM bytes for callers that need cert/key as files
// (the router listener flags take file paths, not in-memory certs).
type TestLeaf struct {
	TLSCert tls.Certificate
	Cert    *x509.Certificate
	CertPEM []byte
	KeyPEM  []byte
}

// IssueLeaf signs a new leaf certificate valid for 127.0.0.1 and "localhost"
// — every httptest server in these suites binds to 127.0.0.1, and the
// pinning path never does a DNS lookup, so those are the only names a test
// upstream ever needs. serial must be unique per leaf issued from the same
// CA.
func (ca *TestCA) IssueLeaf(t *testing.T, serial int64) TestLeaf {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("build tls.Certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse leaf cert: %v", err)
	}
	tlsCert.Leaf = leaf
	return TestLeaf{TLSCert: tlsCert, Cert: leaf, CertPEM: certPEM, KeyPEM: keyPEM}
}

// WriteFiles persists the leaf's cert/key as PEM files and returns their
// paths, for callers (the router listener) that take file paths rather than
// an in-memory tls.Certificate.
func (l TestLeaf) WriteFiles(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	dir := t.TempDir()
	certPath = filepath.Join(dir, "leaf.pem")
	keyPath = filepath.Join(dir, "leaf-key.pem")
	if err := os.WriteFile(certPath, l.CertPEM, 0o600); err != nil {
		t.Fatalf("write leaf cert: %v", err)
	}
	if err := os.WriteFile(keyPath, l.KeyPEM, 0o600); err != nil {
		t.Fatalf("write leaf key: %v", err)
	}
	return certPath, keyPath
}

// FingerprintSHA256 renders a certificate's DER-SHA-256 fingerprint the same
// way normalizePin expects it: lowercase hex, no colons.
func FingerprintSHA256(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}
