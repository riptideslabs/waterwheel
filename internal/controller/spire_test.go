// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"go.riptides.io/waterwheel/api/v1alpha1"
	"go.riptides.io/waterwheel/internal/bindings"
	"go.riptides.io/waterwheel/internal/controller"
)

const (
	trustDomain = "waterwheel.test"
	saKey       = "k8s:pod:serviceaccount"
)

var clusterSPIFFEID = schema.GroupVersionKind{Group: "spire.spiffe.io", Version: "v1alpha1", Kind: "ClusterSPIFFEID"}

func newWID(name, id string, entries ...map[string]string) *v1alpha1.WorkloadIdentity {
	w := &v1alpha1.WorkloadIdentity{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       v1alpha1.WorkloadIdentitySpec{WorkloadID: id},
	}

	for _, e := range entries {
		sel := v1alpha1.WorkloadSelector{}
		for k, v := range e {
			sel[k] = intstr.FromString(v)
		}

		w.Spec.Selectors = append(w.Spec.Selectors, sel)
	}

	return w
}

func TestMutateSPIRE(t *testing.T) {
	t.Parallel()

	i := newInjector(t)
	i.CSIDriver = "csi.spiffe.io"

	pod := appPod(nil)
	if err := i.Mutate(t.Context(), namespace, pod); err != nil {
		t.Fatal(err)
	}

	var (
		tokens []string
		csi    string
	)

	for _, v := range pod.Spec.Volumes {
		switch {
		case v.Projected != nil:
			for _, s := range v.Projected.Sources {
				tokens = append(tokens, s.ServiceAccountToken.Audience)
			}
		case v.CSI != nil:
			csi = v.Name

			if v.CSI.Driver != "csi.spiffe.io" || v.CSI.ReadOnly == nil || !*v.CSI.ReadOnly {
				t.Errorf("CSI volume %+v", v.CSI)
			}
		}
	}

	if !slices.Equal(tokens, []string{controller.Audience}) {
		t.Errorf("token audiences = %v; want only the controller's", tokens)
	}

	mounted := func(c corev1.Container) bool {
		return slices.ContainsFunc(c.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == csi })
	}

	sidecar, app := pod.Spec.InitContainers[0], pod.Spec.Containers[0]

	if csi == "" || !mounted(sidecar) || mounted(app) {
		t.Error("the Workload API must be mounted into the sidecar, and only there")
	}

	env := map[string]string{}
	for _, e := range sidecar.Env {
		env[e.Name] = e.Value
	}

	if env["WATERWHEEL_IDENTITY_SOURCE"] != "spire" ||
		env["SPIFFE_ENDPOINT_SOCKET"] != "unix://"+controller.WorkloadAPIDir+"/spire-agent.sock" {
		t.Errorf("sidecar env = %v", env)
	}

	if _, ok := pod.Annotations[controller.AnnotationAudiences]; ok {
		t.Error("with SPIRE, a pod's audiences aren't fixed")
	}
}

type labeledAuth map[string]string

func (l labeledAuth) Authenticate(context.Context, string) (controller.Caller, error) {
	return controller.Caller{Namespace: namespace, ServiceAccount: serviceAccount, Pod: podName, Labels: l}, nil
}

func snapshotOf(t *testing.T, api *controller.API) bindings.Snapshot {
	t.Helper()

	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	resp := get(t, srv.URL+"/v1/bindings", goodToken)
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}

	var snap bindings.Snapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatal(err)
	}

	return snap
}

// twoIdentities: one WorkloadIdentity for the bot's service account, with a
// binding, and one for its team label.
func twoIdentities(t *testing.T) client.Client {
	t.Helper()

	b := newBinding("bot")
	b.Spec.WorkloadID = "ns/support/bot"

	return newClient(t,
		&v1alpha1.CredentialSource{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: sourceName},
			Spec:       v1alpha1.CredentialSourceSpec{Anthropic: &v1alpha1.AnthropicWIFCredentialProvider{}},
		},
		b,
		newWID("bot", "ns/support/bot", map[string]string{saKey: serviceAccount}),
		newWID("by-label", "ns/support/other", map[string]string{"k8s:label:team": serviceAccount}),
	)
}

func TestAPISPIRE(t *testing.T) {
	t.Parallel()

	c := twoIdentities(t)

	api := &controller.API{
		Auth: labeledAuth(nil), Reader: c, SystemNamespace: systemNamespace,
		Changes: controller.NewBroadcaster(), TrustDomain: trustDomain,
	}

	snap := snapshotOf(t, api)
	if snap.SPIFFEID != "spiffe://waterwheel.test/ns/support/bot" || len(snap.Bindings) != 1 || snap.Reason != "" {
		t.Errorf("snapshot = %+v", snap)
	}

	// Both identities match, and neither is more specific.
	api.Auth = labeledAuth{"team": serviceAccount}

	snap = snapshotOf(t, api)
	if snap.SPIFFEID != "" || len(snap.Bindings) != 0 || snap.Reason == "" {
		t.Errorf("ambiguous: snapshot = %+v", snap)
	}
}

func TestRegistrar(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(clusterSPIFFEID, meta.RESTScopeRoot)

	stale := &unstructured.Unstructured{}
	stale.SetGroupVersionKind(clusterSPIFFEID)
	stale.SetName("waterwheel.support.deleted.0")
	stale.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "waterwheel-controller"})

	foreign := stale.DeepCopy()
	foreign.SetName("spire-server-spire-default")
	foreign.SetLabels(nil)

	w := newWID("bot", "ns/support/bot",
		map[string]string{saKey: serviceAccount, "k8s:label:team": serviceAccount},
		map[string]string{"k8s:pod:name": podName})
	same := newWID("same", "ns/support/same", map[string]string{"k8s:pod:name": podName})
	same.CreationTimestamp = metav1.Now()

	c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).
		WithObjects(w, same, stale, foreign).WithStatusSubresource(&v1alpha1.WorkloadIdentity{}).Build()

	rec := &recorder{}
	r := &controller.IdentityRegistrar{
		Client: c, Reader: c, Changes: controller.NewBroadcaster(),
		SystemNamespace: systemNamespace, TrustDomain: trustDomain, ClassName: "spire-server-spire", Recorder: rec,
	}

	if err := r.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}

	recorded := reasons(rec.take())
	slices.Sort(recorded)

	if !slices.Equal(recorded, []string{"Normal Registered", "Warning SelectorsOverlap"}) {
		t.Errorf("events %v", recorded)
	}

	// A second sync changes nothing, so it records nothing.
	if err := r.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}

	if got := reasons(rec.take()); len(got) != 0 {
		t.Errorf("events %v without a change", got)
	}

	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(clusterSPIFFEID.GroupVersion().WithKind("ClusterSPIFFEIDList"))

	if err := c.List(t.Context(), &list); err != nil {
		t.Fatal(err)
	}

	got := map[string]map[string]any{}
	for _, o := range list.Items {
		got[o.GetName()], _, _ = unstructured.NestedMap(o.Object, "spec")
	}

	if _, ok := got["waterwheel.support.deleted.0"]; ok {
		t.Error("the stale ClusterSPIFFEID wasn't deleted")
	}

	if _, ok := got["spire-server-spire-default"]; !ok {
		t.Error("a ClusterSPIFFEID Waterwheel didn't write was deleted")
	}

	if _, ok := got["waterwheel.support.same.0"]; ok {
		t.Error("the newer identity with the same selectors was registered")
	}

	first := got["waterwheel.support.bot.0"]
	if first["spiffeIDTemplate"] != "spiffe://{{ .TrustDomain }}/ns/support/bot" || first["className"] != "spire-server-spire" {
		t.Errorf("spec = %v", first)
	}

	if ns, _, _ := unstructured.NestedString(first, "namespaceSelector", "matchLabels", "kubernetes.io/metadata.name"); ns != namespace {
		t.Errorf("namespace selector %q", ns)
	}

	if team, _, _ := unstructured.NestedString(first, "podSelector", "matchLabels", "team"); team != serviceAccount {
		t.Errorf("pod selector %q", team)
	}

	if sel, _, _ := unstructured.NestedStringSlice(got["waterwheel.support.bot.1"], "workloadSelectorTemplates"); !slices.Equal(sel,
		[]string{"k8s:container-name:waterwheel-proxy", "k8s:pod-name:" + podName}) {
		t.Errorf("workload selectors %v", sel)
	}

	for name, reason := range map[string]string{"bot": "Registered", "same": "SelectorsOverlap"} {
		var got v1alpha1.WorkloadIdentity
		if err := c.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: name}, &got); err != nil {
			t.Fatal(err)
		}

		if c := meta.FindStatusCondition(got.Status.Conditions, controller.ConditionReady); c == nil || c.Reason != reason {
			t.Errorf("%s: Ready = %+v; want reason %s", name, c, reason)
		}
	}
}

func TestBindingStatusSPIRE(t *testing.T) {
	t.Parallel()

	b := newBinding("bot")
	b.Spec.WorkloadID = "ns/support/bot"

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&v1alpha1.CredentialBinding{}).
		WithObjects(b, &v1alpha1.CredentialSource{
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: sourceName},
			Spec:       v1alpha1.CredentialSourceSpec{Anthropic: &v1alpha1.AnthropicWIFCredentialProvider{}},
		}).Build()

	r := &controller.BindingStatus{Client: c, Pods: c, SystemNamespace: systemNamespace, WorkloadIdentities: true}
	key := client.ObjectKeyFromObject(b)

	ready := func() *metav1.Condition {
		t.Helper()

		if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatal(err)
		}

		var got v1alpha1.CredentialBinding
		if err := c.Get(t.Context(), key, &got); err != nil {
			t.Fatal(err)
		}

		if meta.FindStatusCondition(got.Status.Conditions, controller.ConditionPodsUpToDate) != nil {
			t.Error("with SPIRE, a pod's audiences aren't fixed, so there's no PodsUpToDate")
		}

		return meta.FindStatusCondition(got.Status.Conditions, controller.ConditionReady)
	}

	if c := ready(); c == nil || c.Reason != "NoWorkloadIdentity" {
		t.Errorf("Ready = %+v; want NoWorkloadIdentity", c)
	}

	if err := c.Create(t.Context(), newWID("bot", "ns/support/bot", map[string]string{saKey: serviceAccount})); err != nil {
		t.Fatal(err)
	}

	if c := ready(); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %+v once the identity exists", c)
	}
}
