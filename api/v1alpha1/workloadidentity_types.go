// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// WorkloadIdentity gives the workloads its selectors match the SPIFFE ID
// spiffe://<trust domain>/<workloadID>.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=wid
// +kubebuilder:printcolumn:name="Workload ID",type=string,JSONPath=`.spec.workloadID`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type WorkloadIdentity struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkloadIdentitySpec `json:"spec"`
	Status Status               `json:"status,omitempty"`
}

type WorkloadIdentitySpec struct {
	// WorkloadID is the path of the SPIFFE ID, without a leading slash. Outside
	// the Waterwheel namespace it must start with ns/<namespace>/.
	//
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`
	// +kubebuilder:validation:XValidation:rule="!self.split('/').exists(s, s == '.' || s == '..')",message="path segments must not be . or .."
	WorkloadID string `json:"workloadID"`

	// Selectors match the facts the agent attests about a caller. A caller
	// matches when every key of at least one entry matches.
	//
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	Selectors []WorkloadSelector `json:"selectors"`

	// +optional
	SVID *SVIDProperties `json:"svid,omitempty"`
}

// WorkloadSelector is one selector entry; all its keys must match.
//
// +kubebuilder:validation:MinProperties=1
// +kubebuilder:validation:MaxProperties=16
// +kubebuilder:validation:XValidation:rule="self.all(k, k in ['k8s:pod:namespace', 'k8s:pod:name', 'k8s:pod:serviceaccount', 'k8s:container:name', 'k8s:container:image', 'process:uid', 'process:gid'] || (k.startsWith('k8s:label:') && size(k) > 10))",message="unknown selector key"
type WorkloadSelector map[string]intstr.IntOrString

type SVIDProperties struct {
	// +optional
	X509 *X509Properties `json:"x509,omitempty"`
	// +optional
	JWT *JWTProperties `json:"jwt,omitempty"`
}

type X509Properties struct {
	// TTL of the X.509-SVID. Defaults to 1h.
	//
	// +optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=16
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('10m') && duration(self) <= duration('24h')",message="must be between 10m and 24h"
	TTL *metav1.Duration `json:"ttl,omitempty"`

	// +optional
	// +kubebuilder:validation:MaxItems=100
	// +listType=set
	DNSNames []string `json:"dnsNames,omitempty"`
}

type JWTProperties struct {
	// TTL of the JWT-SVID. Defaults to 5m.
	//
	// +optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=16
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1m') && duration(self) <= duration('1h')",message="must be between 1m and 1h"
	TTL *metav1.Duration `json:"ttl,omitempty"`
}

// Status is shared by all Waterwheel kinds.
type Status struct {
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true

type WorkloadIdentityList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []WorkloadIdentity `json:"items"`
}
