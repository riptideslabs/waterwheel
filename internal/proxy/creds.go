// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"errors"
	"log/slog"
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

// Tokens is what a route needs from its credentials: a valid access token.
type Tokens interface {
	Get(ctx context.Context) (string, error)
}

// credentials keeps a current access token for one binding. tokenex refreshes
// it before expiry; when tokenex reports an error (its channel then closes) it
// backs off and starts over, or retries right away when a request is waiting.
// Ported from the KCD demo's creds.go.
type credentials struct {
	binding bindings.Binding
	subject token.IdentityTokenProvider
	log     *slog.Logger

	mu      sync.Mutex
	access  string
	expiry  time.Time
	lastErr error
	changed chan struct{} // closed and replaced on every result
	kick    chan struct{}
}

func newCredentials(b bindings.Binding, subject token.IdentityTokenProvider, log *slog.Logger) *credentials {
	return &credentials{
		binding: b,
		subject: subject,
		log:     log,
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

	return c.lastErr != nil && strings.Contains(c.lastErr.Error(), "status 4")
}

func (c *credentials) set(access string, expiry time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.access, c.expiry, c.lastErr = access, expiry, nil
	close(c.changed)
	c.changed = make(chan struct{})

	c.log.Info("got an access token", "token", redact(access), "expiresIn", until(expiry))
}

func (c *credentials) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.lastErr = err
	close(c.changed)
	c.changed = make(chan struct{})

	c.log.Warn("token exchange failed", "err", describe(err))
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
