// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"

	"go.riptides.io/tokenex/pkg/credential"
	"go.riptides.io/tokenex/pkg/token"
	"go.riptides.io/waterwheel/internal/bindings"
)

var (
	errExchangeTimeout = errors.New("timed out waiting for a token exchange")
	errNoToken         = errors.New("no valid token")
)

// reportGrace holds back the report of a first failure that may fix itself:
// SPIRE issues a new pod's SVIDs a few seconds late.
const reportGrace = 30 * time.Second

// Tokens is what a route needs from its credentials: a valid access token.
type Tokens interface {
	Get(ctx context.Context) (string, error)
}

// credentials keeps a current access token for one binding. tokenex refreshes
// it before expiry; when tokenex reports an error (its channel then closes) it
// backs off and starts over, or retries right away when a request is waiting.
// It reports when it first gets a token, starts failing, fails differently,
// or recovers. Ported from the KCD demo's creds.go.
type credentials struct {
	binding bindings.Binding
	subject token.IdentityTokenProvider
	log     *slog.Logger
	report  func(bindings.Report)
	started time.Time

	mu       sync.Mutex
	access   string
	expiry   time.Time
	lastErr  error
	reported string        // the last report's reason and, for a failure, its error
	changed  chan struct{} // closed and replaced on every result
	kick     chan struct{}
}

func newCredentials(b bindings.Binding, subject token.IdentityTokenProvider, log *slog.Logger,
	report func(bindings.Report),
) *credentials {
	return &credentials{
		binding: b,
		subject: subject,
		log:     log,
		report:  report,
		started: time.Now(),
		changed: make(chan struct{}),
		kick:    make(chan struct{}, 1),
	}
}

func (c *credentials) run(ctx context.Context) {
	backoff := 2 * time.Second
	tokenexLog := logr.FromSlogHandler(c.log.Handler())

	for ctx.Err() == nil {
		ch, err := Exchange(ctx, tokenexLog, c.binding, c.subject)
		if err != nil {
			c.fail(err)
		} else {
			for res := range ch {
				if res.Err != nil {
					c.fail(res.Err)

					break
				}

				if tok, ok := res.Credential.(*credential.Oauth2Creds); ok {
					c.set(tok.AccessToken, tok.Expiry)

					backoff = 2 * time.Second
				}
			}
		}

		// A request that arrived during the failed attempt already got this result.
		select {
		case <-c.kick:
		default:
		}

		// A denied exchange won't succeed on its own, so only retry it for a request.
		var retry <-chan time.Time
		if !c.denied() {
			retry = time.After(backoff)
		}

		select {
		case <-ctx.Done():
			return
		case <-c.kick:
		case <-retry:
			backoff = min(backoff*2, time.Minute)
		}
	}
}

func (c *credentials) denied() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return denied(c.lastErr)
}

// refusal finds a 4xx status in an exchange error: tokenex's "token endpoint
// returned status 401" and "token endpoint error 400: invalid_grant", Google's
// "Error 403" and "response code 403", and Azure's "RESPONSE 400".
var refusal = regexp.MustCompile(`(?i)\b(?:status|error|response(?: code)?) (4\d\d)\b`)

// denied: the token endpoint refused the exchange. A timeout or a rate limit
// isn't a refusal.
func denied(err error) bool {
	if err == nil {
		return false
	}

	m := refusal.FindStringSubmatch(err.Error())

	return m != nil && m[1] != "408" && m[1] != "429"
}

func (c *credentials) set(access string, expiry time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.access, c.expiry, c.lastErr = access, expiry, nil
	close(c.changed)
	c.changed = make(chan struct{})

	c.log.Info("got an access token", "token", redact(access), "expiresIn", until(expiry))

	if c.reported != bindings.ReasonCredentialIssued {
		c.reported = bindings.ReasonCredentialIssued
		c.send(bindings.ReasonCredentialIssued,
			fmt.Sprintf("%s issued a token, valid for %s", ProviderName(c.binding), until(expiry)))
	}
}

func (c *credentials) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.lastErr = err
	close(c.changed)
	c.changed = make(chan struct{})

	msg := describe(err)
	c.log.Warn("token exchange failed", "err", msg)

	if c.reported == "" && !denied(err) && time.Since(c.started) < reportGrace {
		return
	}

	if key := bindings.ReasonTokenExchangeFailed + ": " + msg; c.reported != key {
		c.reported = key
		c.send(bindings.ReasonTokenExchangeFailed,
			fmt.Sprintf("token exchange with %s failed: %s", ProviderName(c.binding), msg))
	}
}

func (c *credentials) send(reason, msg string) {
	if c.report != nil {
		c.report(bindings.Report{Binding: c.binding.Namespace + "/" + c.binding.Name, Reason: reason, Message: msg})
	}
}

func (c *credentials) valid() (string, bool) {
	if c.access != "" && time.Until(c.expiry) > 5*time.Second {
		return c.access, true
	}

	return "", false
}

// Get returns a valid access token. Without one it asks for a new exchange and
// returns that attempt's result.
func (c *credentials) Get(ctx context.Context) (string, error) {
	c.mu.Lock()
	if tok, ok := c.valid(); ok {
		c.mu.Unlock()

		return tok, nil
	}

	changed := c.changed
	c.mu.Unlock()

	select {
	case c.kick <- struct{}{}:
	default:
	}

	select {
	case <-changed:
	case <-time.After(15 * time.Second):
		return "", errExchangeTimeout
	case <-ctx.Done():
		return "", ctx.Err()
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if tok, ok := c.valid(); ok {
		return tok, nil
	}

	if c.lastErr == nil {
		return "", errNoToken
	}

	return "", c.lastErr
}

// failed is the credentials of a binding that has no subject token.
type failed struct{ err error }

func (f failed) Get(context.Context) (string, error) { return "", f.err }

// describe is the first line of an error; some tokenex errors carry a stack trace.
func describe(err error) string {
	first, _, _ := strings.Cut(err.Error(), "\n")

	return first
}

func redact(tok string) string {
	if len(tok) < 20 {
		return "(short token)"
	}

	return tok[:8] + "…" + tok[len(tok)-4:]
}

func until(t time.Time) time.Duration {
	return time.Until(t).Round(time.Second)
}
