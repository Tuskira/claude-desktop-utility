package ca

import (
	"crypto/x509"
	"os"
	"testing"
)

func TestLeafVerifiesAgainstCA(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir, false); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(c.Cert)

	for _, host := range []string{"api.example.com", "127.0.0.1", "::1"} {
		leaf, err := c.LeafFor(host)
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		if _, err := leaf.Leaf.Verify(x509.VerifyOptions{
			Roots:     pool,
			DNSName:   host,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}); err != nil {
			t.Errorf("%s: verify: %v", host, err)
		}
		again, _ := c.LeafFor(host)
		if again != leaf {
			t.Errorf("%s: leaf not cached", host)
		}
	}
	leaf, _ := c.LeafFor("api.example.com")
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "other.example.com"}); err == nil {
		t.Error("leaf verified for a wrong DNS name")
	}
}

func TestInitRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir, false); err != nil {
		t.Fatal(err)
	}
	if err := Init(dir, false); err == nil {
		t.Fatal("second Init without force should fail")
	}
	if err := Init(dir, true); err != nil {
		t.Fatalf("Init with force: %v", err)
	}
	st, err := os.Stat(KeyPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("key perm = %v, want 0600", st.Mode().Perm())
	}
	if !Exists(dir) {
		t.Error("Exists = false")
	}
}
