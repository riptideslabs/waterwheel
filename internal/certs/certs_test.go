// SPDX-License-Identifier: Apache-2.0

package certs_test

import (
	"bytes"
	"crypto/x509"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"go.riptides.io/waterwheel/internal/certs"
)

func TestEnsure(t *testing.T) {
	t.Parallel()

	c := fake.NewClientBuilder().Build()
	names := []string{"waterwheel-controller.waterwheel-system.svc"}

	first, err := certs.Ensure(t.Context(), c, "waterwheel-system", "waterwheel-tls", names)
	if err != nil {
		t.Fatal(err)
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(first.CAPEM) {
		t.Fatal("CA PEM doesn't parse")
	}

	if _, err := first.Cert.Leaf.Verify(x509.VerifyOptions{DNSName: names[0], Roots: roots}); err != nil {
		t.Fatalf("serving cert doesn't verify against its CA: %v", err)
	}

	// A second replica gets the same certificate from the Secret.
	again, err := certs.Ensure(t.Context(), c, "waterwheel-system", "waterwheel-tls", names)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(again.Cert.Leaf.Raw, first.Cert.Leaf.Raw) {
		t.Error("the second call issued a new certificate")
	}

	// A new name gets a new leaf from the same CA, so proxies keep trusting it.
	more := []string{names[0], "waterwheel-controller.waterwheel-system.svc.cluster.local"}

	renamed, err := certs.Ensure(t.Context(), c, "waterwheel-system", "waterwheel-tls", more)
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Equal(renamed.Cert.Leaf.Raw, first.Cert.Leaf.Raw) || !bytes.Equal(renamed.CAPEM, first.CAPEM) {
		t.Error("a new name should reissue the leaf with the same CA")
	}
}
