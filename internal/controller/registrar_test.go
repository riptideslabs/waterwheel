// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"slices"
	"strings"
	"testing"
	"text/template"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"go.riptides.io/waterwheel/api/v1alpha1"
	"go.riptides.io/waterwheel/internal/controller"
	ids "go.riptides.io/waterwheel/internal/identity"
)

// SPIRE's controller manager renders these fields as templates. A value from
// a WorkloadIdentity must come out as it went in.
func TestClusterSPIFFEIDsKeepValuesLiteral(t *testing.T) {
	t.Parallel()

	const action = "{{ .PodMeta.Name }}"

	r := &controller.IdentityRegistrar{ClassName: "spire"}
	objs := r.ClusterSPIFFEIDs(&ids.Identity{
		Namespace: namespace, Name: "w", WorkloadID: "ns/support/" + action,
		Entries: []ids.Entry{{ids.KeyPodName: action, ids.KeyServiceAccount: action}},
		SVID:    &v1alpha1.SVIDProperties{X509: &v1alpha1.X509Properties{DNSNames: []string{action + ".support.svc"}}},
	})

	spec, _, _ := unstructured.NestedMap(objs[0].Object, "spec")
	selectors, _, _ := unstructured.NestedStringSlice(spec, "workloadSelectorTemplates")
	dnsNames, _, _ := unstructured.NestedStringSlice(spec, "dnsNameTemplates")
	id, _, _ := unstructured.NestedString(spec, "spiffeIDTemplate")

	render := func(tpl string) string {
		t.Helper()

		parsed, err := template.New("").Parse(tpl)
		if err != nil {
			t.Fatalf("%s: %v", tpl, err)
		}

		var out strings.Builder
		if err := parsed.Execute(&out, map[string]any{
			"TrustDomain": "example.org",
			"PodMeta":     map[string]string{"Name": "another-pod"},
		}); err != nil {
			t.Fatalf("%s: %v", tpl, err)
		}

		return out.String()
	}

	rendered := make([]string, 0, 2+len(selectors))
	rendered = append(rendered, render(id), render(dnsNames[0]))

	for _, s := range selectors {
		rendered = append(rendered, render(s))
	}

	slices.Sort(rendered)

	want := []string{
		"k8s:container-name:waterwheel-proxy",
		"k8s:pod-name:" + action,
		"k8s:sa:" + action,
		"spiffe://example.org/ns/support/" + action,
		action + ".support.svc",
	}
	slices.Sort(want)

	if !slices.Equal(rendered, want) {
		t.Errorf("rendered\n %v\nwant\n %v", rendered, want)
	}
}
