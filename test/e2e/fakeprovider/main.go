// SPDX-License-Identifier: Apache-2.0

// Command fakeprovider stands in for a provider in Waterwheel's e2e test, so it
// runs offline. It serves two hosts:
//
//	sts.fake.waterwheel.test  an RFC 8693 token endpoint that accepts the cluster's
//	                          service account tokens, checked against the API
//	                          server's keys, or with JWKS_URL set, SPIRE's
//	                          JWT-SVIDs, checked against the keys there; it
//	                          issues opaque access tokens
//	api.fake.waterwheel.test  an Anthropic-shaped API that answers 401 without one
//	                          of those tokens, and a greeting with one
package main

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"k8s.io/client-go/rest"
)

const (
	tokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	accessToken   = "urn:ietf:params:oauth:token-type:access_token"
	tokenTTL      = 5 * time.Minute
	errorKey      = "error"
	typeKey       = "type"
)

var errBadIssuer = errors.New("API server returned no issuer")

type server struct {
	api      *rest.Config
	http     *http.Client
	audience string
	allowed  []string
	// jwksURL and issuer are set to check JWT-SVIDs rather than service
	// account tokens: SPIRE's OIDC discovery provider serves its keys.
	jwksURL string
	issuer  string

	mu     sync.Mutex
	issued map[string]grant
}

type grant struct {
	subject string
	expiry  time.Time
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fakeprovider:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return err
	}

	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return err
	}

	s := &server{
		api:      cfg,
		http:     hc,
		audience: cmp.Or(os.Getenv("AUDIENCE"), "https://sts.fake.waterwheel.test/token"),
		allowed:  strings.Split(os.Getenv("ALLOWED_SUBJECTS"), ","),
		jwksURL:  os.Getenv("JWKS_URL"),
		issuer:   os.Getenv("ISSUER"),
		issued:   map[string]grant{},
	}

	srv := &http.Server{
		Addr:              cmp.Or(os.Getenv("LISTEN_ADDR"), ":8443"),
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
	}

	slog.Info("listening", "addr", srv.Addr, "audience", s.audience, "allowed", s.allowed, "jwks", s.jwksURL)

	return srv.ListenAndServeTLS("/tls/tls.crt", "/tls/tls.key")
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}

	switch {
	case strings.HasPrefix(host, "sts."):
		s.token(w, r)
	case strings.HasPrefix(host, "api."):
		s.messages(w, r)
	default:
		http.NotFound(w, r)
	}
}

// token is the RFC 8693 token exchange.
func (s *server) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/token" {
		http.NotFound(w, r)

		return
	}

	req, err := exchangeRequest(r)
	if err != nil || req["grant_type"] != tokenExchange {
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "want a token exchange")

		return
	}

	sub, err := s.verify(req["subject_token"])
	if err != nil {
		slog.Info("exchange refused", "err", err)
		oauthError(w, http.StatusUnauthorized, "invalid_grant", err.Error())

		return
	}

	b := make([]byte, 16)
	_, _ = rand.Read(b)
	tok := "fake-" + hex.EncodeToString(b)

	s.mu.Lock()
	s.issued[tok] = grant{subject: sub, expiry: time.Now().Add(tokenTTL)}
	s.mu.Unlock()

	slog.Info("issued a token", "subject", sub)

	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":      tok,
		"issued_token_type": accessToken,
		"token_type":        "Bearer",
		"expires_in":        int(tokenTTL.Seconds()),
	})
}

// exchangeRequest reads the request's parameters. RFC 8693 says form
// encoding; tokenex sends JSON, as OpenAI's endpoint takes. Accept both.
func exchangeRequest(r *http.Request) (map[string]string, error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var req map[string]string
		err := json.NewDecoder(r.Body).Decode(&req)

		return req, err
	}

	if err := r.ParseForm(); err != nil {
		return nil, err
	}

	req := map[string]string{}
	for k := range r.PostForm {
		req[k] = r.PostForm.Get(k)
	}

	return req, nil
}

// verify checks a subject token against its issuer's keys, and returns its
// subject.
func (s *server) verify(raw string) (string, error) {
	tok, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256, jose.ES256, jose.ES384})
	if err != nil {
		return "", err
	}

	issuer, keys, err := s.keys()
	if err != nil {
		return "", err
	}

	var claims jwt.Claims
	if err := tok.Claims(keys, &claims); err != nil {
		return "", err
	}

	expected := jwt.Expected{Issuer: issuer, AnyAudience: jwt.Audience{s.audience}, Time: time.Now()}
	if err := claims.ValidateWithLeeway(expected, time.Minute); err != nil {
		return "", err
	}

	if !slices.Contains(s.allowed, claims.Subject) {
		return "", fmt.Errorf("subject %s is not allowed", claims.Subject) //nolint:err113 // a test server's message
	}

	return claims.Subject, nil
}

// keys returns the issuer and the keys of the tokens it accepts: SPIRE's from
// JWKS_URL, or the API server's.
func (s *server) keys() (string, jose.JSONWebKeySet, error) {
	var keys jose.JSONWebKeySet

	if s.jwksURL != "" {
		err := getJSON(http.DefaultClient, s.jwksURL, &keys)

		return s.issuer, keys, err
	}

	var discovery struct {
		Issuer string `json:"issuer"`
	}
	if err := s.get("/.well-known/openid-configuration", &discovery); err != nil {
		return "", keys, err
	}

	if discovery.Issuer == "" {
		return "", keys, errBadIssuer
	}

	err := s.get("/openid/v1/jwks", &keys)

	return discovery.Issuer, keys, err
}

func (s *server) get(path string, v any) error {
	return getJSON(s.http, strings.TrimSuffix(s.api.Host, "/")+path, v)
}

func getJSON(c *http.Client, url string, v any) error {
	resp, err := c.Get(url) //nolint:noctx // a test server
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status) //nolint:err113 // a test server's message
	}

	return json.NewDecoder(resp.Body).Decode(v)
}

// messages answers like the Anthropic Messages API: 401 without a token this
// server issued, a greeting with one.
func (s *server) messages(w http.ResponseWriter, r *http.Request) {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")

	s.mu.Lock()
	g, ok := s.issued[tok]
	s.mu.Unlock()

	if !ok || time.Now().After(g.expiry) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			typeKey:  errorKey,
			errorKey: map[string]string{typeKey: "authentication_error", "message": "invalid x-api-key"},
		})

		return
	}

	slog.Info("answered", "subject", g.subject, "path", r.URL.Path)

	writeJSON(w, http.StatusOK, map[string]any{
		"id":      "msg_fake",
		typeKey:   "message",
		"role":    "assistant",
		"content": []map[string]string{{typeKey: "text", "text": "Hello from the fake provider, " + g.subject + "!"}},
	})
}

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{errorKey: code, "error_description": desc})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Debug("writing a response", "err", err)
	}
}
