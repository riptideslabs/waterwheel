// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"go.riptides.io/waterwheel/api/v1alpha1"
	"go.riptides.io/waterwheel/internal/bindings"
)

var (
	errTimeout = errors.New("dial tcp: i/o timeout")
	errDenied  = errors.New("status 401: invalid_grant")
)

// reports collects the reasons of a credentials' reports.
func reports(t *testing.T) (*credentials, *[]string) {
	t.Helper()

	b := bindings.Binding{
		Namespace: "support", Name: "support-bot-anthropic",
		Provider: v1alpha1.CredentialSourceSpec{Anthropic: &v1alpha1.AnthropicWIFCredentialProvider{}},
	}

	var got []string

	c := newCredentials(b, nil, slog.New(slog.DiscardHandler), func(r bindings.Report) {
		if r.Binding != "support/support-bot-anthropic" {
			t.Errorf("report about %q", r.Binding)
		}

		got = append(got, r.Reason)
	})

	return c, &got
}

func TestCredentialsReportChanges(t *testing.T) {
	t.Parallel()

	c, got := reports(t)
	expiry := time.Now().Add(time.Hour)

	c.fail(errTimeout) // held back: it may fix itself
	c.set("tok-1", expiry)
	c.set("tok-2", expiry) // a refresh
	c.fail(errTimeout)
	c.fail(errTimeout) // the same failure again
	c.fail(errDenied)
	c.set("tok-3", expiry)

	want := []string{
		bindings.ReasonCredentialIssued,
		bindings.ReasonTokenExchangeFailed,
		bindings.ReasonTokenExchangeFailed,
		bindings.ReasonCredentialIssued,
	}
	if !slices.Equal(*got, want) {
		t.Errorf("reports %v; want %v", *got, want)
	}
}

func TestCredentialsReportFirstFailure(t *testing.T) {
	t.Parallel()

	// A denied exchange won't fix itself, so it's reported right away.
	denied, got := reports(t)
	denied.fail(errDenied)

	if len(*got) != 1 {
		t.Errorf("denied: reports %v", *got)
	}

	// Other failures are reported once they last.
	lasting, got := reports(t)
	lasting.started = time.Now().Add(-reportGrace)
	lasting.fail(errTimeout)

	if len(*got) != 1 {
		t.Errorf("lasting: reports %v", *got)
	}
}
