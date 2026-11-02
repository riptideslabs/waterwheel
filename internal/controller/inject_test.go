// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"encoding/json"
	"slices"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"go.riptides.io/waterwheel/api/v1alpha1"
	"go.riptides.io/waterwheel/internal/bindings"
	"go.riptides.io/waterwheel/internal/controller"
)

const anthropicAudience = "https://api.anthropic.com"

func newInjector(t *testing.T) *controller.Injector {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	return &controller.Injector{
		Decoder: admission.NewDecoder(scheme),
		Reader: newClient(t,
			&v1alpha1.CredentialSource{
				ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: sourceName},
				Spec:       v1alpha1.CredentialSourceSpec{Anthropic: &v1alpha1.AnthropicWIFCredentialProvider{}},
			},
			newBinding("bot-anthropic"),
		),
		SystemNamespace: systemNamespace,
		ProxyImage:      "waterwheel-proxy:test",
		PullPolicy:      corev1.PullIfNotPresent,
		ControllerAddr:  "waterwheel-controller.waterwheel-system.svc:8443",
		ControllerCA:    []byte("CA"),
		TokenTTL:        3600,
	}
}

func appPod(annotations map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, GenerateName: "bot-", Annotations: annotations},
		Spec: corev1.PodSpec{
			ServiceAccountName: serviceAccount,
			Containers: []corev1.Container{{
				Name:  "app",
				Image: "app",
				Env:   []corev1.EnvVar{{Name: "SSL_CERT_FILE", Value: "/mine.pem"}},
			}},
		},
	}
}

func TestMutate(t *testing.T) {
	t.Parallel()

	pod := appPod(nil)
	if err := newInjector(t).Mutate(t.Context(), namespace, pod); err != nil {
		t.Fatal(err)
	}

	sidecar := pod.Spec.InitContainers[0]
	if sidecar.Name != controller.SidecarName || sidecar.RestartPolicy == nil || *sidecar.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Fatalf("first init container = %s; want the native waterwheel-proxy sidecar", sidecar.Name)
	}

	var tokens []string

	for _, v := range pod.Spec.Volumes {
		if v.Projected == nil {
			continue
		}

		for _, s := range v.Projected.Sources {
			if s.ServiceAccountToken.Path != bindings.TokenFile(s.ServiceAccountToken.Audience) {
				t.Errorf("token for %s at %s", s.ServiceAccountToken.Audience, s.ServiceAccountToken.Path)
			}

			tokens = append(tokens, s.ServiceAccountToken.Audience)
		}
	}

	slices.Sort(tokens)

	if !slices.Equal(tokens, []string{anthropicAudience, controller.Audience}) {
		t.Errorf("token audiences = %v", tokens)
	}

	app := pod.Spec.Containers[0]
	env := map[string]string{}

	for _, e := range app.Env {
		env[e.Name] = e.Value
	}

	switch {
	case env["HTTPS_PROXY"] != "http://"+controller.ProxyAddr:
		t.Errorf("HTTPS_PROXY = %q", env["HTTPS_PROXY"])
	case env["SSL_CERT_FILE"] != "/mine.pem":
		t.Errorf("the app's own SSL_CERT_FILE was overridden: %q", env["SSL_CERT_FILE"])
	case env["CURL_CA_BUNDLE"] != controller.BundleDir+"/"+controller.BundleFile:
		t.Errorf("CURL_CA_BUNDLE = %q", env["CURL_CA_BUNDLE"])
	}

	mounts := map[string]corev1.VolumeMount{}
	for _, m := range app.VolumeMounts {
		mounts[m.MountPath] = m
	}

	if m, ok := mounts["/etc/ssl/certs/ca-certificates.crt"]; !ok || m.SubPath != controller.BundleFile {
		t.Error("the bundle isn't mounted over the Debian/Alpine bundle path")
	}

	if _, ok := mounts[controller.TokenDir]; ok {
		t.Error("the app container can see the tokens")
	}

	if got := pod.Annotations[controller.AnnotationAudiences]; got != anthropicAudience {
		t.Errorf("audiences annotation = %q", got)
	}
}

func TestMutateEnvOnly(t *testing.T) {
	t.Parallel()

	pod := appPod(map[string]string{controller.AnnotationCABundle: "env-only"})
	if err := newInjector(t).Mutate(t.Context(), namespace, pod); err != nil {
		t.Fatal(err)
	}

	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.SubPath != "" {
			t.Errorf("env-only pod got a bundle mount at %s", m.MountPath)
		}
	}
}

func TestHandle(t *testing.T) {
	t.Parallel()

	inj := newInjector(t)

	tests := []struct {
		name        string
		annotations map[string]string
		patched     bool
	}{
		{name: "opted in", patched: true},
		{name: "opted out", annotations: map[string]string{controller.AnnotationInject: "false"}},
		{name: "already injected", annotations: map[string]string{controller.AnnotationInjected: "true"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			raw, err := json.Marshal(appPod(tt.annotations))
			if err != nil {
				t.Fatal(err)
			}

			resp := inj.Handle(t.Context(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
				Namespace: namespace,
				Object:    runtime.RawExtension{Raw: raw},
			}})

			if !resp.Allowed || (len(resp.Patches) > 0) != tt.patched {
				t.Errorf("allowed = %v, %d patches; want patched = %v", resp.Allowed, len(resp.Patches), tt.patched)
			}
		})
	}
}

func TestMutateExtraRoots(t *testing.T) {
	t.Parallel()

	inj := newInjector(t)
	inj.ConfigMaps = fake.NewClientBuilder().WithObjects(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: systemNamespace, Name: controller.ExtraRootsConfigMap},
		Data:       map[string]string{"ca.crt": "-----BEGIN CERTIFICATE-----"},
	}).Build()

	pod := appPod(nil)
	if err := inj.Mutate(t.Context(), namespace, pod); err != nil {
		t.Fatal(err)
	}

	if !slices.ContainsFunc(pod.Spec.InitContainers[0].Env, func(e corev1.EnvVar) bool {
		return e.Name == "WATERWHEEL_EXTRA_ROOTS" && e.Value == "-----BEGIN CERTIFICATE-----"
	}) {
		t.Error("the sidecar didn't get the extra roots")
	}
}
