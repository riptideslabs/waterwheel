// SPDX-License-Identifier: Apache-2.0

// Package proxy is waterwheel-proxy: the per-pod CA, the combined CA bundle the
// app trusts, and (to come) the CONNECT proxy that adds credentials.
package proxy

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/x509roots/fallback/bundle"
)

// caTTL outlives any pod; leaves are short.
const (
	caTTL   = 365 * 24 * time.Hour
	leafTTL = 24 * time.Hour
)

var errNotECDSA = errors.New("CA key is not ECDSA")

// CA is the pod's interception CA. Its key lives in a memory-backed volume only
// the sidecar mounts, so a sidecar restart keeps the CA the app already
// trusts, and the key disappears with the pod.
type CA struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
	PEM  []byte

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

// LoadOrCreateCA loads the CA from dir, or creates one there.
func LoadOrCreateCA(dir string) (*CA, error) {
	certPath, keyPath := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")

	if ca, err := loadCA(certPath, keyPath); err == nil {
		return ca, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	host, _ := os.Hostname()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "waterwheel-proxy CA for " + host},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(caTTL),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := errors.Join(
		os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600),
		os.WriteFile(certPath, certPEM, 0o600),
	); err != nil {
		return nil, err
	}

	return loadCA(certPath, keyPath)
}

func loadCA(certPath, keyPath string) (*CA, error) {
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}

	key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errNotECDSA
	}

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, err
	}

	return &CA{Cert: pair.Leaf, Key: key, PEM: certPEM, leaves: map[string]*tls.Certificate{}}, nil
}

// Leaf returns a serving certificate for host, cached while it has an hour left.
func (c *CA) Leaf(host string) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if l, ok := c.leaves[host]; ok && time.Until(l.Leaf.NotAfter) > time.Hour {
		return l, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(leafTTL),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.Cert, &key.PublicKey, c.Key)
	if err != nil {
		return nil, err
	}

	l := &tls.Certificate{Certificate: [][]byte{der, c.Cert.Raw}, PrivateKey: key}
	if l.Leaf, err = x509.ParseCertificate(der); err != nil {
		return nil, err
	}

	c.leaves[host] = l

	return l, nil
}

// WriteBundle writes the CA bundle the app trusts: the public roots, any extra
// roots, and the pod CA. The app mounts the file over its own bundle, so it
// must keep trusting everything the proxy passes through untouched. The file is
// rewritten in place, because subPath mounts keep pointing at the old inode.
func WriteBundle(path string, podCA, extra []byte) error {
	buf, err := roots(extra)
	if err != nil {
		return err
	}

	buf.Write(podCA)

	return os.WriteFile(path, buf.Bytes(), 0o644) //nolint:gosec // public certificates the app must read
}

// WriteRoots writes what the proxy itself trusts upstream: the public roots and
// any extra roots, without the pod CA.
func WriteRoots(path string, extra []byte) error {
	buf, err := roots(extra)
	if err != nil {
		return err
	}

	return os.WriteFile(path, buf.Bytes(), 0o600)
}

func roots(extra []byte) (*bytes.Buffer, error) {
	var buf bytes.Buffer

	for r := range bundle.Roots() {
		// A PEM bundle can't carry a distrust-after date, so leave those roots
		// out rather than trust them without it.
		if r.Constraint != nil {
			continue
		}

		if err := pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: r.Certificate}); err != nil {
			return nil, err
		}
	}

	if len(bytes.TrimSpace(extra)) > 0 {
		buf.Write(bytes.TrimSpace(extra))
		buf.WriteByte('\n')
	}

	return &buf, nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))

	return n
}
