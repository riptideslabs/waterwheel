// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"go.riptides.io/waterwheel/api/v1alpha1"
)

// An X.509-SVID may only carry DNS names of its own namespace's Services:
// another namespace's Service name in a certificate from the cluster's CA
// would pass for that Service.
func TestDNSNamesStayInTheNamespace(t *testing.T) {
	t.Parallel()

	testCases := map[string]struct {
		namespace     string
		clusterDomain string
		names         []string
		foreign       bool
	}{
		"own services":          {namespace: "a", names: []string{"db.a.svc", "DB.a.svc.cluster.local", "*.a.svc"}},
		"another namespace":     {namespace: "a", names: []string{"db.a.svc", "postgres.billing.svc.cluster.local"}, foreign: true},
		"another short name":    {namespace: "a", names: []string{"postgres.billing.svc"}, foreign: true},
		"outside the cluster":   {namespace: "a", names: []string{"db.a.svc.attacker.example"}, foreign: true},
		"no service name":       {namespace: "a", names: []string{"a.svc"}, foreign: true},
		"system namespace":      {namespace: testSystem, names: []string{"postgres.billing.svc.cluster.local"}},
		"own cluster domain":    {namespace: "a", clusterDomain: "corp.example", names: []string{"db.a.svc.corp.example"}},
		"not the cluster's own": {namespace: "a", clusterDomain: "corp.example", names: []string{"db.a.svc.cluster.local"}, foreign: true},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			w := testWID(t, tc.namespace, "w", "ns/"+tc.namespace+"/w", 1, []map[string]string{{KeyServiceAccount: "w"}})
			w.Spec.SVID = &v1alpha1.SVIDProperties{X509: &v1alpha1.X509Properties{DNSNames: tc.names}}

			b := &Builder{SystemNamespace: testSystem, ClusterDomain: tc.clusterDomain}
			set := b.Build([]v1alpha1.WorkloadIdentity{w})

			problem, rejected := set.Problems[types.NamespacedName{Namespace: tc.namespace, Name: "w"}]

			switch {
			case tc.foreign && problem.Reason != ReasonForeignDNSName:
				t.Errorf("in effect with %v (problem %v)", tc.names, problem)
			case !tc.foreign && rejected:
				t.Errorf("not in effect: %s", problem.Message)
			}
		})
	}
}
