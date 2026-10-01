// Package ca creates a local certificate authority and mints per-host leaf
// certificates signed by it.
package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// CertFile and KeyFile are the file names used inside the CA directory.
	CertFile = "ca.pem"
	KeyFile  = "ca-key.pem"
	// CommonName is the subject CN of the generated CA.
	CommonName = "Interceptor Local CA"
)

// CA holds a loaded CA and a cache of minted leaf certificates.
type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

// CertPath returns the CA certificate path inside dir.
func CertPath(dir string) string { return filepath.Join(dir, CertFile) }

// KeyPath returns the CA private key path inside dir.
func KeyPath(dir string) string { return filepath.Join(dir, KeyFile) }

// Exists reports whether both CA files exist in dir.
func Exists(dir string) bool {
	for _, p := range []string{CertPath(dir), KeyPath(dir)} {
		if _, err := os.Stat(p); err != nil {
			return false
		}
	}
	return true
}

// Init generates a new CA in dir. It refuses to overwrite existing files
// unless force is set.
func Init(dir string, force bool) error {
	cp, kp := CertPath(dir), KeyPath(dir)
	if !force {
		for _, p := range []string{cp, kp} {
			if _, err := os.Stat(p); err == nil {
				return fmt.Errorf("%s already exists (use --force to overwrite)", p)
			}
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := newSerial()
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: CommonName, Organization: []string{"Interceptor"}},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := writeFile(kp, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	// The certificate is public; 0644 lets apps run by other users read it.
	return writeFile(cp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func writeFile(path string, data []byte, perm os.FileMode) error {
	if err := os.WriteFile(path, data, perm); err != nil {
		return err
	}
	return os.Chmod(path, perm)
}

// Load reads the CA certificate and key from dir.
func Load(dir string) (*CA, error) {
	certPEM, err := os.ReadFile(CertPath(dir))
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(KeyPath(dir))
	if err != nil {
		return nil, err
	}
	cb, _ := pem.Decode(certPEM)
	if cb == nil {
		return nil, fmt.Errorf("%s: no PEM block", CertPath(dir))
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, fmt.Errorf("%s: no PEM block", KeyPath(dir))
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", KeyPath(dir), err)
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return nil, errors.New("CA certificate and key do not match")
	}
	return &CA{Cert: cert, Key: key, leaves: map[string]*tls.Certificate{}}, nil
}

func newSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

func normalizeHost(h string) string {
	h = strings.TrimSpace(h)
	h = strings.TrimPrefix(strings.TrimSuffix(h, "]"), "[")
	h = strings.TrimSuffix(h, ".")
	return strings.ToLower(h)
}

// LeafFor returns a certificate for host (DNS name or IP), minting and
// caching it on first use.
func (c *CA) LeafFor(host string) (*tls.Certificate, error) {
	host = normalizeHost(host)
	if host == "" {
		return nil, errors.New("empty host")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if cert, ok := c.leaves[host]; ok && time.Until(cert.Leaf.NotAfter) > 24*time.Hour {
		return cert, nil
	}
	cert, err := c.mint(host)
	if err != nil {
		return nil, err
	}
	c.leaves[host] = cert
	return cert, nil
}

// GetCertificate is a tls.Config.GetCertificate callback keyed on SNI.
func (c *CA) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	return c.LeafFor(hello.ServerName)
}

func (c *CA) mint(host string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	if len(host) <= 64 {
		tmpl.Subject = pkix.Name{CommonName: host}
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.Cert, &key.PublicKey, c.Key)
	if err != nil {
		return nil, fmt.Errorf("mint cert for %q: %w", host, err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}
