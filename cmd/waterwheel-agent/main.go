// SPDX-License-Identifier: Apache-2.0

// Command waterwheel-agent runs on every node. It serves the SPIFFE Workload API
// on a Unix socket, attests each caller (SO_PEERCRED → cgroup → pod via the
// kubelet), matches it against WorkloadIdentity objects, and gets its SVIDs
// from waterwheel-controller. It also streams the caller's CredentialBindings to
// waterwheel-proxy. See docs/design.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.riptides.io/waterwheel/internal/version"
)

func main() {
	var (
		socket     = flag.String("socket", "/run/waterwheel/agent.sock", "Workload API socket")
		controller = flag.String("controller", "waterwheel-controller.waterwheel-system.svc:8443", "agent API of waterwheel-controller")
		nodeName   = flag.String("node-name", os.Getenv("NODE_NAME"), "this node's name")
		healthAddr = flag.String("health-addr", ":8082", "address of /healthz")
	)
	flag.Parse()

	if err := run(*socket, *controller, *nodeName, *healthAddr); err != nil {
		fmt.Fprintln(os.Stderr, "waterwheel-agent:", err)
		os.Exit(1)
	}
}

func run(socket, controller, nodeName, healthAddr string) error {
	if nodeName == "" {
		return errors.New("--node-name or NODE_NAME is required")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// TODO(M1): bootstrap with the controller, attest callers, serve the Workload API.
	slog.Info("starting", "version", version.Version(), "socket", socket, "controller", controller, "node", nodeName)

	return serveHealth(ctx, healthAddr)
}

func serveHealth(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	if err := srv.ListenAndServe(); err != nil && ctx.Err() == nil {
		return err
	}

	return nil
}
