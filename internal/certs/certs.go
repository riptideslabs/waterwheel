// SPDX-License-Identifier: Apache-2.0

// Package certs keeps the controller's serving certificate, used by the
// webhook and the bindings API, in a Secret. Every replica serves the same
// chain, and the CA can be handed to the API server and to proxies.
package certs

import (
	"context"
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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	caTTL     = 10 * 365 * 24 * time.Hour
	leafTTL   = 365 * 24 * time.Hour
	renewAt   = 30 * 24 * time.Hour // renew the leaf when it has less left than this
	caCertKey = "ca.crt"
	caPrivKey = "ca.key" // only the controller can read the Secret (see the chart's Role)
)

var errUnusable = errors.New("unusable serving certificate")

// Serving is the controller's TLS certificate and the CA that signed it.
type Serving struct {
	Cert  tls.Certificate
	CAPEM []byte
}

// Ensure loads the serving certificate from the Secret, or makes one (and the
// CA, if the Secret has none) and stores it there. The certificate covers
// dnsNames.
func Ensure(ctx context.Context, c client.Client, namespace, name string, dnsNames []string) (*Serving, error) {
	var sec corev1.Secret

	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &sec)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}

	if err == nil {
		if s, err := load(sec.Data, dnsNames); err == nil {
			return s, nil
		}
	}

	data, err := issue(sec.Data, dnsNames)
	if err != nil {
		return nil, err
	}

	if sec.Name == "" {
		sec = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
			Type:       corev1.SecretTypeTLS,
			Data:       data,
		}
		if err := c.Create(ctx, &sec); apierrors.IsAlreadyExists(err) {
			// Another replica was first; use its certificate.
			return Ensure(ctx, c, namespace, name, dnsNames)
		} else if err != nil {
			return nil, err
		}
	} else {
		sec.Data = data
		if err := c.Update(ctx, &sec); err != nil {
			return nil, err
		}
	}

	return load(data, dnsNames)
}

func load(data map[string][]byte, dnsNames []string) (*Serving, error) {
	cert, err := tls.X509KeyPair(data[corev1.TLSCertKey], data[corev1.TLSPrivateKeyKey])
	if err != nil {
		return nil, err
	}

	leaf := cert.Leaf
	if time.Until(leaf.NotAfter) < renewAt {
		return nil, fmt.Errorf("%w: expires %s", errUnusable, leaf.NotAfter)
	}

	for _, n := range dnsNames {
		if leaf.VerifyHostname(n) != nil {
			return nil, fmt.Errorf("%w: doesn't cover %s", errUnusable, n)
		}
	}

	if len(data[caCertKey]) == 0 {
		return nil, fmt.Errorf("%w: no CA", errUnusable)
	}

	return &Serving{Cert: cert, CAPEM: data[caCertKey]}, nil
}

// issue signs a new leaf, with the existing CA when there is a usable one.
func issue(old map[string][]byte, dnsNames []string) (map[string][]byte, error) {
	caCert, caPriv, caPEM, caKeyPEM, err := loadCA(old)
	if err != nil {
		if caCert, caPriv, caPEM, caKeyPEM, err = newCA(); err != nil {
			return nil, err
		}
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     dnsNames,
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(leafTTL),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caPriv)
	if err != nil {
		return nil, err
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}

	return map[string][]byte{
		corev1.TLSCertKey:       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		corev1.TLSPrivateKeyKey: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
		caCertKey:               caPEM,
		caPrivKey:               caKeyPEM,
	}, nil
}

func loadCA(data map[string][]byte) (*x509.Certificate, *ecdsa.PrivateKey, []byte, []byte, error) {
	pair, err := tls.X509KeyPair(data[caCertKey], data[caPrivKey])
	if err != nil {
		return nil, nil, nil, nil, err
	}

	priv, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok || time.Until(pair.Leaf.NotAfter) < leafTTL {
		return nil, nil, nil, nil, fmt.Errorf("%w: CA", errUnusable)
	}

	return pair.Leaf, priv, data[caCertKey], data[caPrivKey], nil
}

func newCA() (*x509.Certificate, *ecdsa.PrivateKey, []byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "waterwheel-controller CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(caTTL),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, nil, nil, err
	}

	return cert, key,
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))

	return n
}
