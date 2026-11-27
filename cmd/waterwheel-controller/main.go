// SPDX-License-Identifier: Apache-2.0

// Command waterwheel-controller is Waterwheel's control plane. It injects waterwheel-proxy
// into pods with the projected tokens they need, serves each proxy its
// CredentialBindings, authenticated by TokenReview, and keeps the bindings'
// status. With the spire source, it also registers WorkloadIdentities with
// SPIRE. See docs/design.md.
package main

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"go.riptides.io/waterwheel/api/v1alpha1"
	"go.riptides.io/waterwheel/internal/certs"
	"go.riptides.io/waterwheel/internal/controller"
	"go.riptides.io/waterwheel/internal/identity"
	"go.riptides.io/waterwheel/internal/version"
)

type config struct {
	source         string
	trustDomain    string
	spireClassName string
	clusterDomain  string
	csiDriver      string
	issuer         string
	namespace      string
	serviceName    string
	apiAddr        string
	healthAddr     string
	webhookAddr    string
	webhookConfig  string
	proxyImage     string
	pullPolicy     string
	tokenTTL       int64
}

func main() {
	var cfg config

	flag.StringVar(&cfg.source, "identity-source", "serviceAccountToken", "what proxies present to providers: serviceAccountToken, spire or waterwheel")
	flag.StringVar(&cfg.trustDomain, "trust-domain", "", "spire and waterwheel sources: SPIFFE trust domain, e.g. example.org")
	flag.StringVar(&cfg.spireClassName, "spire-class-name", "spire-server-spire", "spire source: className of the ClusterSPIFFEIDs Waterwheel writes")
	flag.StringVar(&cfg.csiDriver, "spiffe-csi-driver", "csi.spiffe.io", "spire source: the CSI driver that mounts the SPIRE agent's socket")
	flag.StringVar(&cfg.clusterDomain, "cluster-domain", "cluster.local", "the cluster's DNS domain: its Services bypass the proxy, and X.509-SVID DNS names must be under a namespace's own Services")
	flag.StringVar(&cfg.issuer, "issuer", "", "waterwheel source: iss claim of JWT-SVIDs; the OIDC discovery URL")
	flag.StringVar(&cfg.namespace, "namespace", cmp.Or(os.Getenv("POD_NAMESPACE"), "waterwheel-system"), "Waterwheel's own namespace; its bindings apply to any namespace")
	flag.StringVar(&cfg.serviceName, "service-name", "waterwheel-controller", "the controller's Service, for its serving certificate")
	flag.StringVar(&cfg.apiAddr, "api-addr", ":8443", "address of the bindings API (HTTPS)")
	flag.StringVar(&cfg.healthAddr, "health-addr", ":8082", "address of /healthz and /readyz")
	flag.StringVar(&cfg.webhookAddr, "webhook-addr", ":9443", "address of the injection webhook (HTTPS)")
	flag.StringVar(&cfg.webhookConfig, "webhook-config", "waterwheel-injector", "the MutatingWebhookConfiguration to write the CA into")
	flag.StringVar(&cfg.proxyImage, "proxy-image", "waterwheel-proxy:dev", "image of the injected sidecar")
	flag.StringVar(&cfg.pullPolicy, "proxy-pull-policy", string(corev1.PullIfNotPresent), "pull policy of the injected sidecar")
	flag.Int64Var(&cfg.tokenTTL, "token-ttl", 3600, "lifetime of the sidecar's projected tokens, in seconds")
	flag.Parse()

	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "waterwheel-controller:", err)
		os.Exit(1)
	}
}

func run(cfg config) error {
	spire := cfg.source == "spire"

	switch {
	case cfg.source == "waterwheel":
		return errors.New("the waterwheel identity source is not implemented yet")
	case spire && cfg.trustDomain == "":
		return errors.New("the spire identity source needs --trust-domain")
	case !spire && cfg.source != "serviceAccountToken":
		return fmt.Errorf("unknown --identity-source %q", cfg.source)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	ctrl.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))

	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return err
	}

	scheme := runtime.NewScheme()
	if err := errors.Join(clientgoscheme.AddToScheme(scheme), v1alpha1.AddToScheme(scheme)); err != nil {
		return err
	}

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress:  cfg.healthAddr,
		LeaderElection:          true,
		LeaderElectionID:        "waterwheel-controller",
		LeaderElectionNamespace: cfg.namespace,
		// The process exits when the manager stops, so hand the lease over
		// right away; a rollout then doesn't wait for it to expire.
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		return err
	}

	// For TokenReviews and the certificate Secret: not cached, and usable
	// before the manager starts.
	direct, err := client.New(restCfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}

	// Renews the certificate while the controller runs, too.
	keeper := &certs.Keeper{Client: direct, Namespace: cfg.namespace, Name: "waterwheel-tls", DNSNames: []string{
		cfg.serviceName + "." + cfg.namespace + ".svc",
		cfg.serviceName + "." + cfg.namespace + ".svc.cluster.local",
	}}

	serving, err := keeper.Ensure(ctx)
	if err != nil {
		return fmt.Errorf("serving certificate: %w", err)
	}

	changes := controller.NewBroadcaster()
	if err := notifyOnChange(ctx, mgr.GetCache(), changes); err != nil {
		return err
	}

	tlsConfig := &tls.Config{GetCertificate: keeper.GetCertificate, MinVersion: tls.VersionTLS12}
	service := cfg.serviceName + "." + cfg.namespace + ".svc"

	injector := &controller.Injector{
		Decoder:         admission.NewDecoder(scheme),
		Reader:          mgr.GetClient(),
		SystemNamespace: cfg.namespace,
		ProxyImage:      cfg.proxyImage,
		PullPolicy:      corev1.PullPolicy(cfg.pullPolicy),
		ControllerAddr:  service + ":8443",
		ControllerCA:    serving.CAPEM,
		TokenTTL:        cfg.tokenTTL,
		ConfigMaps:      mgr.GetAPIReader(),
		ClusterDomain:   cfg.clusterDomain,
	}

	if spire {
		injector.CSIDriver = cfg.csiDriver
	}

	webhookMux := http.NewServeMux()
	webhookMux.Handle("/inject", &admission.Webhook{Handler: injector})

	webhook := &controller.TLSServer{Server: controller.NewWebhookServer(cfg.webhookAddr, webhookMux, tlsConfig)}
	webhookCA := &controller.WebhookCA{Client: direct, Name: cfg.webhookConfig, CAPEM: serving.CAPEM}

	recorder := mgr.GetEventRecorder("waterwheel-controller")
	// Shared, so the identities in effect are worked out once per change.
	identities := &identity.Builder{SystemNamespace: cfg.namespace, ClusterDomain: cfg.clusterDomain}

	api := &controller.API{
		Auth:            &controller.TokenReviewer{Client: direct, Pods: mgr.GetAPIReader()},
		Reader:          mgr.GetClient(),
		SystemNamespace: cfg.namespace,
		Changes:         changes,
		Recorder:        recorder,
		Identities:      identities,
	}

	if spire {
		api.TrustDomain = cfg.trustDomain
	}

	server := &controller.TLSServer{Server: controller.NewAPIServer(cfg.apiAddr, api, tlsConfig)}

	status := &controller.BindingStatus{
		Client:                mgr.GetClient(),
		Pods:                  mgr.GetAPIReader(),
		SystemNamespace:       cfg.namespace,
		ServiceAccountIDsOnly: !spire,
		WorkloadIdentities:    spire,
		Identities:            identities,
		Recorder:              recorder,
	}

	if err := errors.Join(
		mgr.Add(server),
		mgr.Add(webhook),
		mgr.Add(webhookCA),
		mgr.Add(keeper),
		status.SetupWithManager(mgr),
		mgr.AddHealthzCheck("ping", healthz.Ping),
		mgr.AddReadyzCheck("webhook-ca", webhookCA.Ready),
	); err != nil {
		return err
	}

	if spire {
		registrar := &controller.IdentityRegistrar{
			Client:          mgr.GetClient(),
			Reader:          mgr.GetAPIReader(),
			Changes:         changes,
			SystemNamespace: cfg.namespace,
			TrustDomain:     cfg.trustDomain,
			ClassName:       cfg.spireClassName,
			Identities:      identities,
			Recorder:        recorder,
		}

		if err := mgr.Add(registrar); err != nil {
			return err
		}
	}

	slog.Info("starting", "version", version.Version(), "identitySource", cfg.source, "namespace", cfg.namespace, "api", cfg.apiAddr, "webhook", cfg.webhookAddr)

	return mgr.Start(ctx)
}

// notifyOnChange wakes bindings watchers, and the registrar, whenever a
// binding, source or identity changes.
func notifyOnChange(ctx context.Context, c cache.Cache, b *controller.Broadcaster) error {
	notify := toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { b.Notify() },
		UpdateFunc: func(any, any) { b.Notify() },
		DeleteFunc: func(any) { b.Notify() },
	}

	for _, obj := range []client.Object{&v1alpha1.CredentialBinding{}, &v1alpha1.CredentialSource{}, &v1alpha1.WorkloadIdentity{}} {
		inf, err := c.GetInformer(ctx, obj, cache.BlockUntilSynced(false))
		if err != nil {
			return err
		}

		if _, err := inf.AddEventHandler(notify); err != nil {
			return err
		}
	}

	return nil
}
