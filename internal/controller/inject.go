// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"cmp"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"go.riptides.io/waterwheel/internal/bindings"
)

// Pod annotations.
const (
	// AnnotationInject set to "false" keeps the sidecar out of a pod.
	AnnotationInject = "waterwheel.riptides.io/inject"
	// AnnotationCABundle set to "env-only" skips mounting the CA bundle over
	// the OS bundle paths; the environment variables still point at it.
	AnnotationCABundle = "waterwheel.riptides.io/ca-bundle"
	// AnnotationInjected marks injected pods.
	AnnotationInjected = "waterwheel.riptides.io/injected"
	// AnnotationAudiences lists the projected token audiences a pod got,
	// comma-separated. Bindings that need another one wait for a restart.
	AnnotationAudiences = "waterwheel.riptides.io/audiences"
)

// SidecarName is the injected container.
const SidecarName = "waterwheel-proxy"

// strTrue is the value of boolean annotations and query parameters.
const strTrue = "true"

// Paths and addresses inside the pod.
const (
	ProxyAddr  = "127.0.0.1:3128"
	TokenDir   = "/run/waterwheel-proxy/tokens"
	BundleDir  = "/run/waterwheel-proxy/bundle"
	BundleFile = "ca-bundle.crt"
	CAKeyDir   = "/run/waterwheel-proxy/ca-key"

	tokenVolume  = "waterwheel-proxy-tokens"
	bundleVolume = "waterwheel-proxy-bundle"
	caKeyVolume  = "waterwheel-proxy-ca-key"
)

// osBundlePaths are where Linux distributions keep their CA bundle.
var osBundlePaths = []string{
	"/etc/ssl/certs/ca-certificates.crt",                // Debian, Ubuntu, Alpine
	"/etc/ssl/cert.pem",                                 // Alpine
	"/etc/pki/tls/certs/ca-bundle.crt",                  // RHEL, Fedora
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", // RHEL, Fedora
	"/etc/ssl/ca-bundle.pem",                            // SUSE
}

// bundleEnv are the variables common runtimes read a CA bundle from.
var bundleEnv = []string{
	"SSL_CERT_FILE",                    // OpenSSL, Go, Python's ssl, Ruby
	"CURL_CA_BUNDLE",                   // curl
	"REQUESTS_CA_BUNDLE",               // Python requests
	"NODE_EXTRA_CA_CERTS",              // Node.js
	"AWS_CA_BUNDLE",                    // AWS SDKs and CLI
	"GRPC_DEFAULT_SSL_ROOTS_FILE_PATH", // gRPC C core
}

// Injector adds waterwheel-proxy to pods in namespaces that turned injection on
// (the webhook's namespaceSelector does the matching). The sidecar gets one
// projected service account token per audience the pod's bindings need, plus
// one for the bindings API; the app containers get none.
type Injector struct {
	Decoder         admission.Decoder
	Reader          client.Reader
	SystemNamespace string
	ProxyImage      string
	PullPolicy      corev1.PullPolicy
	// ControllerAddr is the bindings API, host:port.
	ControllerAddr string
	// ControllerCA verifies the bindings API's certificate.
	ControllerCA []byte
	// TokenTTL is the projected tokens' lifetime; the kubelet replaces them
	// before they expire.
	TokenTTL int64
	// ConfigMaps reads the waterwheel-extra-roots ConfigMap in the system
	// namespace; an uncached reader, so the controller doesn't cache every
	// ConfigMap.
	ConfigMaps client.Reader
}

// ExtraRootsConfigMap holds CAs (key ca.crt) that both the proxy, upstream,
// and the app trust on top of the public roots: a corporate TLS proxy's, or a
// private provider's.
const ExtraRootsConfigMap = "waterwheel-extra-roots"

func (i *Injector) Handle(ctx context.Context, req admission.Request) admission.Response {
	pod := &corev1.Pod{}
	if err := i.Decoder.Decode(req, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	switch {
	case pod.Annotations[AnnotationInject] == "false":
		return admission.Allowed("opted out")
	case pod.Annotations[AnnotationInjected] == strTrue || hasContainer(pod, SidecarName):
		return admission.Allowed("already injected")
	}

	if err := i.Mutate(ctx, req.Namespace, pod); err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	raw, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	slog.Info("injected "+SidecarName, "namespace", req.Namespace, "pod", cmp.Or(pod.Name, pod.GenerateName+"…"),
		"audiences", pod.Annotations[AnnotationAudiences])

	return admission.PatchResponseFromRaw(req.Object.Raw, raw)
}

// Mutate adds the sidecar, its volumes, and the proxy and CA settings of the
// app containers to a pod in namespace.
func (i *Injector) Mutate(ctx context.Context, namespace string, pod *corev1.Pod) error {
	sa := cmp.Or(pod.Spec.ServiceAccountName, "default")

	snap, err := bindings.For(ctx, i.Reader, i.SystemNamespace, namespace, bindings.ServiceAccountID(namespace, sa))
	if err != nil {
		return err
	}

	auds := snap.Audiences()

	extra, err := i.extraRoots(ctx)
	if err != nil {
		return err
	}

	pod.Spec.InitContainers = append([]corev1.Container{i.sidecar(extra)}, pod.Spec.InitContainers...)
	pod.Spec.Volumes = append(pod.Spec.Volumes, i.volumes(auds)...)

	bundle := BundleDir + "/" + BundleFile
	env := []corev1.EnvVar{
		{Name: "HTTPS_PROXY", Value: "http://" + ProxyAddr},
		{Name: "NO_PROXY", Value: "localhost,127.0.0.1,.svc,.cluster.local"},
	}

	for _, name := range bundleEnv {
		env = append(env, corev1.EnvVar{Name: name, Value: bundle})
	}

	osPaths := pod.Annotations[AnnotationCABundle] != "env-only"

	// Point every app container at the proxy and the bundle, without
	// overriding anything it sets itself.
	for c := range pod.Spec.Containers {
		ctr := &pod.Spec.Containers[c]

		for _, e := range env {
			if !hasEnv(ctr, e.Name) {
				ctr.Env = append(ctr.Env, e)
			}
		}

		ctr.VolumeMounts = append(ctr.VolumeMounts, corev1.VolumeMount{Name: bundleVolume, MountPath: BundleDir, ReadOnly: true})

		if !osPaths {
			continue
		}

		for _, p := range osBundlePaths {
			if !hasMount(ctr, p) {
				ctr.VolumeMounts = append(ctr.VolumeMounts, corev1.VolumeMount{
					Name: bundleVolume, MountPath: p, SubPath: BundleFile, ReadOnly: true,
				})
			}
		}
	}

	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}

	pod.Annotations[AnnotationInjected] = strTrue
	pod.Annotations[AnnotationAudiences] = strings.Join(auds, ",")

	return nil
}

func (i *Injector) extraRoots(ctx context.Context) (string, error) {
	if i.ConfigMaps == nil {
		return "", nil
	}

	var cm corev1.ConfigMap

	err := i.ConfigMaps.Get(ctx, client.ObjectKey{Namespace: i.SystemNamespace, Name: ExtraRootsConfigMap}, &cm)
	if apierrors.IsNotFound(err) {
		return "", nil
	}

	return cm.Data["ca.crt"], err
}

func (i *Injector) sidecar(extraRoots string) corev1.Container {
	always := corev1.ContainerRestartPolicyAlways
	probe := &corev1.Probe{
		ProbeHandler:  corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{SidecarName, "healthcheck"}}},
		PeriodSeconds: 1,
	}
	startup := *probe
	startup.FailureThreshold = 30

	env := []corev1.EnvVar{{Name: "WATERWHEEL_CONTROLLER_CA", Value: string(i.ControllerCA)}}
	if extraRoots != "" {
		env = append(env, corev1.EnvVar{Name: "WATERWHEEL_EXTRA_ROOTS", Value: extraRoots})
	}

	// A native sidecar: it starts, and passes its startup probe (CA bundle
	// written, listening), before the app containers start.
	return corev1.Container{
		Name:            SidecarName,
		Image:           i.ProxyImage,
		ImagePullPolicy: i.PullPolicy,
		RestartPolicy:   &always,
		Args: []string{
			"--listen=" + ProxyAddr,
			"--controller=" + i.ControllerAddr,
			"--token-dir=" + TokenDir,
			"--ca-bundle-out=" + BundleDir + "/" + BundleFile,
			"--ca-key-dir=" + CAKeyDir,
		},
		Env:            env,
		StartupProbe:   &startup,
		ReadinessProbe: probe,
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("10m"),
			corev1.ResourceMemory: resource.MustParse("32Mi"),
		}},
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                new(int64(65532)),
			RunAsNonRoot:             new(true),
			ReadOnlyRootFilesystem:   new(true),
			AllowPrivilegeEscalation: new(false),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: tokenVolume, MountPath: TokenDir, ReadOnly: true},
			{Name: bundleVolume, MountPath: BundleDir},
			{Name: caKeyVolume, MountPath: CAKeyDir},
		},
	}
}

func (i *Injector) volumes(auds []string) []corev1.Volume {
	tokens := make([]corev1.VolumeProjection, 0, len(auds)+1)
	for _, aud := range append([]string{Audience}, auds...) {
		tokens = append(tokens, corev1.VolumeProjection{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
			Audience: aud, ExpirationSeconds: new(i.TokenTTL), Path: bindings.TokenFile(aud),
		}})
	}

	memory := func(limit string) corev1.VolumeSource {
		return corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			Medium: corev1.StorageMediumMemory, SizeLimit: new(resource.MustParse(limit)),
		}}
	}

	return []corev1.Volume{
		// Mounted only into the sidecar: the app never sees a token.
		{Name: tokenVolume, VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: tokens}}},
		// The combined CA bundle the sidecar writes for the app.
		{Name: bundleVolume, VolumeSource: memory("2Mi")},
		// The pod CA's key, so a sidecar restart keeps the CA the app trusts.
		{Name: caKeyVolume, VolumeSource: memory("1Mi")},
	}
}

func hasContainer(pod *corev1.Pod, name string) bool {
	return slices.ContainsFunc(append(pod.Spec.InitContainers, pod.Spec.Containers...),
		func(c corev1.Container) bool { return c.Name == name })
}

func hasEnv(c *corev1.Container, name string) bool {
	return slices.ContainsFunc(c.Env, func(e corev1.EnvVar) bool { return e.Name == name })
}

func hasMount(c *corev1.Container, path string) bool {
	return slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.MountPath == path })
}
