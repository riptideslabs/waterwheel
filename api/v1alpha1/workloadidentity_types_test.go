// SPDX-License-Identifier: Apache-2.0

package v1alpha1_test

import (
	"encoding/json"
	"testing"

	"go.riptides.io/waterwheel/api/v1alpha1"
)

// The schema allows any integer; one that does not fit an int32 must still
// decode, or the object fails every list of WorkloadIdentities.
func TestWorkloadSelectorDecodesAnyInteger(t *testing.T) {
	t.Parallel()

	const list = `{"items":[
		{"spec":{"workloadID":"ns/a/x","selectors":[{"k8s:pod:name":99999999999,"process:uid":1000,"k8s:label:app":"bot"}]}},
		{"spec":{"workloadID":"ns/b/y","selectors":[{"k8s:pod:serviceaccount":"bot"}]}}
	]}`

	var got v1alpha1.WorkloadIdentityList
	if err := json.Unmarshal([]byte(list), &got); err != nil {
		t.Fatal(err)
	}

	if len(got.Items) != 2 {
		t.Fatalf("%d items; want 2", len(got.Items))
	}

	sel := got.Items[0].Spec.Selectors[0]
	for k, want := range map[string]string{"k8s:pod:name": "99999999999", "process:uid": "1000", "k8s:label:app": "bot"} {
		if v := sel[k]; v.String() != want {
			t.Errorf("%s = %q; want %q", k, v.String(), want)
		}
	}

	out, err := json.Marshal(sel)
	if err != nil {
		t.Fatal(err)
	}

	if want := `{"k8s:label:app":"bot","k8s:pod:name":"99999999999","process:uid":1000}`; string(out) != want {
		t.Errorf("encoded as %s; want %s", out, want)
	}
}
