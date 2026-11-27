// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"go.riptides.io/waterwheel/api/v1alpha1"
)

// SPIRE's controller manager reads a pod or service account name as a
// template: {{ .PodMeta.Name }} would match every pod in the namespace.
func TestPodAndServiceAccountSelectorsTakeNames(t *testing.T) {
	t.Parallel()

	for _, key := range []string{KeyPodName, KeyServiceAccount} {
		for value, valid := range map[string]bool{
			"bot-1":                true,
			"bot.v2":               true,
			"{{ .PodMeta.Name }}":  false,
			"Bot":                  false,
			"bot 1":                false,
			"{{`{{`}} .PodMeta }}": false,
		} {
			w := testWID(t, "a", "w", "ns/a/w", 1, []map[string]string{{key: value}})
			set := Build([]v1alpha1.WorkloadIdentity{w}, testSystem)

			problem, rejected := set.Problems[types.NamespacedName{Namespace: "a", Name: "w"}]

			switch {
			case valid && rejected:
				t.Errorf("%s=%q: %s", key, value, problem.Message)
			case !valid && problem.Reason != ReasonInvalidSelector:
				t.Errorf("%s=%q is in effect (problem %v)", key, value, problem)
			}
		}
	}
}
