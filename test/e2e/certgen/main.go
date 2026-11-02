// SPDX-License-Identifier: Apache-2.0

// Command certgen makes the e2e CA and the fake provider's serving
// certificate in a directory, once: existing files are kept, so the CA baked
// into the test image stays valid across runs.
//
//	certgen <dir>   writes ca.crt, tls.crt and tls.key
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

var errUsage = errors.New("usage: certgen <dir>")

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "certgen:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 2 {
		return errUsage
	}

	dir := os.Args[1]
	if _, err := os.Stat(filepath.Join(dir, "tls.key")); err == nil {
		return nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}

	ca := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "Waterwheel e2e CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}

	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		return err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}

	leaf := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: "fake.waterwheel.test"},
		DNSNames:     []string{"sts.fake.waterwheel.test", "api.fake.waterwheel.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
	if err != nil {
		return err
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}

	return errors.Join(
		os.WriteFile(filepath.Join(dir, "ca.crt"), pemBlock("CERTIFICATE", caDER), 0o644),    //nolint:gosec // public
		os.WriteFile(filepath.Join(dir, "tls.crt"), pemBlock("CERTIFICATE", leafDER), 0o644), //nolint:gosec // public
		os.WriteFile(filepath.Join(dir, "tls.key"), pemBlock("EC PRIVATE KEY", keyDER), 0o600),
	)
}

func pemBlock(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))

	return n
}
