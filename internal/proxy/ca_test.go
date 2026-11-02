// SPDX-License-Identifier: Apache-2.0

package proxy_test

import (
	"bytes"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"

	"go.riptides.io/waterwheel/internal/proxy"
)

func TestCASurvivesRestart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	first, err := proxy.LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}

	again, err := proxy.LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(first.PEM, again.PEM) {
		t.Fatal("a restarted proxy made a new CA; the app would stop trusting it")
	}

	leaf, err := again.Leaf("api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}

	roots := x509.NewCertPool()
	roots.AddCert(first.Cert)

	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{DNSName: "api.anthropic.com", Roots: roots}); err != nil {
		t.Fatalf("leaf doesn't verify against the pod CA: %v", err)
	}
}

func TestWriteBundle(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	ca, err := proxy.LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "ca-bundle.crt")
	if err := proxy.WriteBundle(path, ca.PEM, nil); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		t.Fatal("bundle doesn't parse")
	}

	// The public roots must be there too, or passed-through connections fail.
	if n := bytes.Count(data, []byte("BEGIN CERTIFICATE")); n < 100 {
		t.Errorf("bundle has %d certificates; want the public roots as well", n)
	}

	if !bytes.Contains(data, ca.PEM) {
		t.Error("bundle lacks the pod CA")
	}
}
