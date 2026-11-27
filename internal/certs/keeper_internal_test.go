// SPDX-License-Identifier: Apache-2.0

package certs

import (
	"bytes"
	"crypto/tls"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testNamespace = "waterwheel-system"
	testSecret    = "waterwheel-tls"
)

var testNames = []string{"waterwheel-controller.waterwheel-system.svc"}

// keeperWith is a Keeper serving a leaf with ttl left, as a controller that
// started long ago would, and the Secret it came from.
func keeperWith(t *testing.T, ttl time.Duration) (*Keeper, client.Client) {
	t.Helper()

	data, err := issue(nil, testNames, ttl)
	if err != nil {
		t.Fatal(err)
	}

	cert, err := tls.X509KeyPair(data[corev1.TLSCertKey], data[corev1.TLSPrivateKeyKey])
	if err != nil {
		t.Fatal(err)
	}

	c := fake.NewClientBuilder().WithObjects(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testSecret},
		Data:       data,
	}).Build()

	k := &Keeper{Client: c, Namespace: testNamespace, Name: testSecret, DNSNames: testNames}
	k.current.Store(&Serving{Cert: cert, CAPEM: data[caCertKey]})

	return k, c
}

func served(t *testing.T, k *Keeper) *tls.Certificate {
	t.Helper()

	cert, err := k.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}

	return cert
}

// A controller that keeps running must not serve its leaf until it expires.
func TestKeeperRenewsALeafCloseToItsEnd(t *testing.T) {
	t.Parallel()

	k, _ := keeperWith(t, 10*24*time.Hour)
	old, ca := served(t, k), k.current.Load().CAPEM

	if err := k.renew(t.Context()); err != nil {
		t.Fatal(err)
	}

	cert := served(t, k)

	switch {
	case bytes.Equal(cert.Leaf.Raw, old.Leaf.Raw):
		t.Fatal("still serving the old leaf")
	case time.Until(cert.Leaf.NotAfter) < leafTTL-time.Hour:
		t.Errorf("the new leaf expires %s", cert.Leaf.NotAfter)
	case !bytes.Equal(k.current.Load().CAPEM, ca):
		t.Error("the new leaf is from another CA; the API server and the pods trust the old one")
	}

	// The Secret has it too, for the other replicas.
	again, err := Ensure(t.Context(), k.Client, testNamespace, testSecret, testNames)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(again.Cert.Leaf.Raw, cert.Leaf.Raw) {
		t.Error("the Secret doesn't hold the leaf being served")
	}
}

func TestKeeperKeepsALeafWithTimeLeft(t *testing.T) {
	t.Parallel()

	k, _ := keeperWith(t, leafTTL)
	old := served(t, k)

	if err := k.renew(t.Context()); err != nil {
		t.Fatal(err)
	}

	if served(t, k) != old {
		t.Error("replaced a leaf with a year left")
	}
}

// A Secret with another CA can't be served: nothing trusts that CA yet.
func TestKeeperStopsWhenTheCAChanged(t *testing.T) {
	t.Parallel()

	k, c := keeperWith(t, 10*24*time.Hour)
	old := served(t, k)

	other, err := issue(nil, testNames, leafTTL)
	if err != nil {
		t.Fatal(err)
	}

	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testSecret}}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(sec), sec); err != nil {
		t.Fatal(err)
	}

	sec.Data = other
	if err := c.Update(t.Context(), sec); err != nil {
		t.Fatal(err)
	}

	if err := k.renew(t.Context()); !errors.Is(err, ErrCAChanged) {
		t.Errorf("renew() error = %v; want ErrCAChanged", err)
	}

	if served(t, k) != old {
		t.Error("serving a leaf of the new CA")
	}
}
