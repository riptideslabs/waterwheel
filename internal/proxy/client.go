// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"go.riptides.io/waterwheel/internal/bindings"
)

var errBadCA = errors.New("controller CA doesn't parse")

// BindingsClient watches the controller's bindings API for this pod's bindings.
type BindingsClient struct {
	// Addr is the API, host:port.
	Addr string
	// TokenPath is the pod's projected token for the controller.
	TokenPath string
	HTTP      *http.Client
}

// NewBindingsClient trusts only caPEM, the controller's CA.
func NewBindingsClient(addr, tokenPath string, caPEM []byte) (*BindingsClient, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errBadCA
	}

	return &BindingsClient{
		Addr:      addr,
		TokenPath: tokenPath,
		HTTP: &http.Client{Transport: &http.Transport{
			Proxy:               nil,
			TLSClientConfig:     &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout: 10 * time.Second,
		}},
	}, nil
}

// Watch calls update with every snapshot until ctx ends. The server ends each
// stream after a while; Watch reconnects, with the current token, and backs
// off while the controller is unreachable.
func (c *BindingsClient) Watch(ctx context.Context, update func(bindings.Snapshot)) {
	backoff := time.Second

	for ctx.Err() == nil {
		got, err := c.watchOnce(ctx, update)
		if got {
			backoff = time.Second
		}

		if err != nil && ctx.Err() == nil {
			slog.Warn("bindings watch", "err", err, "retryIn", backoff)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		if !got {
			backoff = min(backoff*2, 30*time.Second)
		}
	}
}

// watchOnce follows one stream; got reports whether it delivered anything.
func (c *BindingsClient) watchOnce(ctx context.Context, update func(bindings.Snapshot)) (bool, error) {
	tok, err := os.ReadFile(c.TokenPath)
	if err != nil {
		return false, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+c.Addr+"/v1/bindings?watch=true", nil)
	if err != nil {
		return false, err
	}

	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("bindings API: %s", resp.Status) //nolint:err113 // carries the status
	}

	got := false
	lines := bufio.NewScanner(resp.Body)
	lines.Buffer(make([]byte, 64*1024), 4*1024*1024)

	for lines.Scan() {
		var snap bindings.Snapshot
		if err := json.Unmarshal(lines.Bytes(), &snap); err != nil {
			return got, err
		}

		update(snap)

		got = true
	}

	return got, lines.Err()
}
