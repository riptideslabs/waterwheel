// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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

func TestDenied(t *testing.T) {
	t.Parallel()

	for msg, want := range map[string]bool{
		"could not exchange token: token endpoint returned status 404: 404 page not found":   true,
		"could not exchange token: token endpoint error 401: invalid_grant — invalid aud":    true,
		"failed to exchange ID token for STS token: googleapi: Error 400: Invalid value":     true,
		"googleapi: got HTTP response code 403 with body: denied":                            true,
		"ClientAssertionCredential authentication failed\nRESPONSE 400: 400 Bad Request":     true,
		"could not exchange token: token endpoint returned status 429: slow down":            false,
		"could not exchange token: token endpoint error 400: temporarily_unavailable — busy": false,
		"could not exchange token: token endpoint error 400: server_error — try again":       false,
		"could not exchange token: token endpoint returned status 503: service unavailable":  false,
		"dial tcp 10.96.0.10:443: i/o timeout":                                               false,
		"open /run/waterwheel-proxy/tokens/aud.token: no such file or directory":             false,
	} {
		if got := denied(errors.New(msg)); got != want { //nolint:err113 // test errors
			t.Errorf("denied(%q) = %v; want %v", msg, got, want)
		}
	}
}

// azureError is an exchange error the way tokenex passes on Azure SDK errors.
func azureError(status int, code, desc, traceID string) error {
	return fmt.Errorf("failed to retrieve credentials: ClientAssertionCredential authentication failed. \n"+ //nolint:err113 // test errors
		"POST https://login.microsoftonline.com/660ef1de-f447-4cf2-b1ba-1194e8f5b0fc/oauth2/v2.0/token\n"+
		"--------------------------------------------------------------------------------\n"+
		"RESPONSE %d: %d %s\n"+
		"--------------------------------------------------------------------------------\n"+
		"{\n  \"error\": %q,\n  \"error_description\": %q,\n  \"trace_id\": %q\n}\n"+
		"--------------------------------------------------------------------------------\n"+
		"To troubleshoot, visit https://aka.ms/azsdk/go/identity/troubleshoot#workload",
		status, status, http.StatusText(status), code,
		desc+"\r\nTrace ID: "+traceID+"\r\nCorrelation ID: 9a1c\r\nTimestamp: 2026-11-09 09:52:22Z", traceID)
}

func TestDescribeAzure(t *testing.T) {
	t.Parallel()

	got := describe(azureError(http.StatusBadRequest, "invalid_client",
		"AADSTS700016: Application with identifier 'support-bot' was not found in the directory 'Riptides'.", "3f0b"))

	want := "failed to retrieve credentials: ClientAssertionCredential authentication failed: status 400: " +
		"invalid_client — AADSTS700016: Application with identifier 'support-bot' was not found in the directory 'Riptides'."
	if got != want {
		t.Errorf("describe() =\n%s\nwant\n%s", got, want)
	}
}

// TestCredentialsReportAzureFailures: Azure failures share their first line,
// so the rest tells them apart; a new trace ID alone doesn't.
func TestCredentialsReportAzureFailures(t *testing.T) {
	t.Parallel()

	c, got := reports(t)
	c.started = time.Now().Add(-reportGrace)

	c.fail(azureError(http.StatusBadRequest, "invalid_client", "AADSTS700016: Application not found.", "3f0b"))
	c.fail(azureError(http.StatusBadRequest, "invalid_client", "AADSTS700016: Application not found.", "77c2"))
	c.fail(azureError(http.StatusUnauthorized, "invalid_grant", "AADSTS70021: No matching federated identity record.", "77c2"))

	want := []string{bindings.ReasonTokenExchangeFailed, bindings.ReasonTokenExchangeFailed}
	if !slices.Equal(*got, want) {
		t.Errorf("reports %v; want %v", *got, want)
	}
}
