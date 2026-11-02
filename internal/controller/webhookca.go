// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var errCANotPublished = errors.New("webhook CA not published yet")

// WebhookCA writes the serving CA into the injector's
// MutatingWebhookConfiguration, so the API server trusts the webhook. It runs
// on every replica; they all publish the same CA.
type WebhookCA struct {
	Client client.Client
	Name   string
	CAPEM  []byte

	published atomic.Bool
}

func (w *WebhookCA) Start(ctx context.Context) error {
	for {
		err := w.publish(ctx)
		if err == nil {
			slog.Info("published the webhook CA", "config", w.Name)
			w.published.Store(true)

			return nil
		}

		slog.Info("publishing the webhook CA, retrying", "config", w.Name, "err", err)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

func (w *WebhookCA) publish(ctx context.Context) error {
	var cfg admissionregistrationv1.MutatingWebhookConfiguration
	if err := w.Client.Get(ctx, client.ObjectKey{Name: w.Name}, &cfg); err != nil {
		return err
	}

	orig := cfg.DeepCopy()
	for i := range cfg.Webhooks {
		cfg.Webhooks[i].ClientConfig.CABundle = w.CAPEM
	}

	return w.Client.Patch(ctx, &cfg, client.MergeFrom(orig))
}

// NeedLeaderElection is false: every replica serves the webhook.
func (w *WebhookCA) NeedLeaderElection() bool { return false }

// Ready is a readyz check: not ready until the CA is published.
func (w *WebhookCA) Ready(*http.Request) error {
	if !w.published.Load() {
		return errCANotPublished
	}

	return nil
}
