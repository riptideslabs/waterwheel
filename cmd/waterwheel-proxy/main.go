// SPDX-License-Identifier: Apache-2.0

// Command waterwheel-proxy is the per-pod sidecar. It is a CONNECT proxy that
// terminates TLS only for destinations the pod has a CredentialBinding for,
// exchanges the pod's projected service account token for the provider's
// token with tokenex, and adds it to each request. Everything else passes
// through as a plain tunnel.
//
//	waterwheel-proxy                    run the proxy
//	waterwheel-proxy healthcheck        exit 0 if the proxy accepts connections
//	waterwheel-proxy token [audience]   print the claims of the pod's projected token
//	                                    for an audience (default: the controller's)
package main

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "golang.org/x/crypto/x509roots/fallback" // the image is FROM scratch

	"go.riptides.io/waterwheel/internal/bindings"
	"go.riptides.io/waterwheel/internal/proxy"
)

var errNotJWT = errors.New("not a JWT")

func main() {
	var (
		listen     = flag.String("listen", "127.0.0.1:3128", "proxy address (HTTPS_PROXY)")
		controller = flag.String("controller", "waterwheel-controller.waterwheel-system.svc:8443", "bindings API of waterwheel-controller")
		tokens     = flag.String("token-dir", "/run/waterwheel-proxy/tokens", "projected service account tokens, one file per audience")
		bundle     = flag.String("ca-bundle-out", "/run/waterwheel-proxy/bundle/ca-bundle.crt", "where to write the combined CA bundle for the app")
		caKeyDir   = flag.String("ca-key-dir", "/run/waterwheel-proxy/ca-key", "where the pod CA's key is kept across sidecar restarts")
	)
	flag.Parse()

	var err error

	switch flag.Arg(0) {
	case "":
		err = run(*listen, *controller, *tokens, *bundle, *caKeyDir)
	case "healthcheck":
		err = healthcheck(*listen)
	case "token":
		err = printToken(*tokens, cmp.Or(flag.Arg(1), "waterwheel-controller"))
	default:
		err = fmt.Errorf("unknown command %q", flag.Arg(0))
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "waterwheel-proxy:", err)
		os.Exit(1)
	}
}

func run(listen, controller, tokens, bundle, caKeyDir string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// The bundle has to exist before the app starts, and the startup probe
	// passes only once the proxy listens, so write it first.
	ca, err := proxy.LoadOrCreateCA(caKeyDir)
	if err != nil {
		return err
	}

	// Extra roots (a corporate TLS proxy's, or a private provider's) go into the
	// app's bundle, and into the roots this process trusts upstream: Go reads
	// SSL_CERT_FILE when it first loads the system roots, which covers tokenex
	// and the cloud SDKs too.
	extra := []byte(os.Getenv("WATERWHEEL_EXTRA_ROOTS"))

	if err := proxy.WriteBundle(bundle, ca.PEM, extra); err != nil {
		return err
	}

	upstreamRoots := filepath.Join(caKeyDir, "upstream-roots.crt")
	if err := proxy.WriteRoots(upstreamRoots, extra); err != nil {
		return err
	}

	if err := os.Setenv("SSL_CERT_FILE", upstreamRoots); err != nil {
		return err
	}

	p := proxy.New(ca, tokens)

	client, err := proxy.NewBindingsClient(controller, filepath.Join(tokens, bindings.TokenFile("waterwheel-controller")),
		[]byte(os.Getenv("WATERWHEEL_CONTROLLER_CA")))
	if err != nil {
		return err
	}

	go client.Watch(ctx, func(snap bindings.Snapshot) { p.Update(ctx, snap) })

	// The app starts once the proxy listens. Wait a while for the bindings,
	// so its first requests already get credentials; until they arrive, every
	// destination is passed through.
	select {
	case <-p.Ready():
	case <-time.After(15 * time.Second):
		slog.Warn("no bindings yet; starting anyway, passing everything through until they arrive")
	case <-ctx.Done():
		return nil
	}

	slog.Info("listening", "addr", listen, "ca", ca.Cert.Subject.CommonName, "caBundle", bundle)

	srv := &http.Server{Addr: listen, Handler: p, ReadHeaderTimeout: 10 * time.Second}

	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	if err := srv.ListenAndServe(); err != nil && ctx.Err() == nil {
		return err
	}

	return nil
}

func healthcheck(listen string) error {
	d := net.Dialer{Timeout: time.Second}

	conn, err := d.DialContext(context.Background(), "tcp", listen)
	if err != nil {
		return err
	}

	return conn.Close()
}

// printToken prints the header and claims of the projected token for an
// audience, without its signature, so the token itself isn't revealed.
func printToken(dir, audience string) error {
	raw, err := os.ReadFile(filepath.Join(dir, bindings.TokenFile(audience)))
	if err != nil {
		return err
	}

	parts := strings.Split(strings.TrimSpace(string(raw)), ".")
	if len(parts) != 3 {
		return errNotJWT
	}

	out := map[string]json.RawMessage{}

	for i, name := range []string{"header", "claims"} {
		b, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil {
			return fmt.Errorf("%w: %w", errNotJWT, err)
		}

		out[name] = b
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")

	return enc.Encode(out)
}
