package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func caPaths() (string, string) {
	return filepath.Join(certDir(), "CoveCA.crt"), filepath.Join(certDir(), "CoveCA.key")
}

// ensureCA creates the local root CA once. Trusting it (certutil, current user
// store — no admin needed) is done by `cove setup` / `cove trust`.
func ensureCA() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	crtPath, keyPath := caPaths()
	if fileExists(crtPath) && fileExists(keyPath) {
		return loadPair(crtPath, keyPath)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	host, _ := os.Hostname()
	tpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "Cove Local CA (" + host + ")", Organization: []string{"Cove local development"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	if err := writePair(crtPath, keyPath, der, key); err != nil {
		return nil, nil, err
	}
	rebuildCABundle()
	return loadPair(crtPath, keyPath)
}

func trustCA() error {
	crt, _ := caPaths()
	if _, _, err := ensureCA(); err != nil {
		return err
	}
	// Shows one Windows confirmation dialog; installs into CurrentUser\Root.
	return exec.Command("certutil", "-user", "-addstore", "Root", crt).Run()
}

func untrustCA() error {
	ca, _, err := ensureCA()
	if err != nil {
		return err
	}
	return exec.Command("certutil", "-user", "-delstore", "Root", ca.SerialNumber.Text(16)).Run()
}

func issueSiteCert(host string) error {
	ca, caKey, err := ensureCA()
	if err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: host, Organization: []string{"Cove"}},
		DNSNames:     []string{host, "*." + host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(0, 0, 820),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	return writePair(filepath.Join(certDir(), host+".crt"), filepath.Join(certDir(), host+".key"), der, key)
}

// ensureSiteCerts (re)issues missing or soon-expiring certs for secured sites.
func ensureSiteCerts() {
	for _, s := range listSites() {
		if !s.Secure {
			continue
		}
		crt := filepath.Join(certDir(), s.Host+".crt")
		if c, _, err := loadPair(crt, filepath.Join(certDir(), s.Host+".key")); err == nil && time.Until(c.NotAfter) > 30*24*time.Hour {
			continue
		}
		issueSiteCert(s.Host)
	}
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n
}

func writePair(crtPath, keyPath string, der []byte, key *ecdsa.PrivateKey) error {
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(crtPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return err
	}
	return os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
}

func loadPair(crtPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cb, err := os.ReadFile(crtPath)
	if err != nil {
		return nil, nil, err
	}
	kb, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}
	cp, _ := pem.Decode(cb)
	kp, _ := pem.Decode(kb)
	if cp == nil || kp == nil {
		return nil, nil, errors.New("invalid PEM")
	}
	c, err := x509.ParseCertificate(cp.Bytes)
	if err != nil {
		return nil, nil, err
	}
	k, err := x509.ParseECPrivateKey(kp.Bytes)
	return c, k, err
}
