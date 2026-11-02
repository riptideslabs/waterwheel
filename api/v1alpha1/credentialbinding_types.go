// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CredentialBinding gives a workload identity the credential from a
// CredentialSource in the same namespace, on requests to the listed destinations.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cb
// +kubebuilder:printcolumn:name="Workload ID",type=string,JSONPath=`.spec.workloadID`
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.credentialSource`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
type CredentialBinding struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CredentialBindingSpec `json:"spec"`
	Status Status                `json:"status,omitempty"`
}

type CredentialBindingSpec struct {
	// +kubebuilder:validation:MaxLength=255
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`
	WorkloadID string `json:"workloadID"`

	// CredentialSource is the name of a CredentialSource in this namespace.
	//
	// +kubebuilder:validation:MinLength=1
	CredentialSource string `json:"credentialSource"`

	Propagation CredentialBindingPropagation `json:"propagation"`
}

type CredentialBindingPropagation struct {
	Injection HTTPHeaderInjection `json:"injection"`
}

// HTTPHeaderInjection adds the token to requests the proxy intercepts.
type HTTPHeaderInjection struct {
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	Destinations []Destination `json:"destinations"`

	// Header defaults to Authorization.
	//
	// +optional
	// +kubebuilder:validation:Pattern=`^[A-Za-z][A-Za-z0-9-]*$`
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="!(self.lowerAscii() in ['host', 'content-length', 'transfer-encoding', 'connection', 'keep-alive', 'proxy-connection', 'te', 'trailer', 'upgrade', 'expect'])",message="must not be a message-framing or connection header"
	Header string `json:"header,omitempty"`

	// Scheme defaults to Bearer. Set it to "-" to send the bare token.
	//
	// +optional
	// +kubebuilder:validation:Pattern=`^([A-Za-z][A-Za-z0-9-]*|-)$`
	// +kubebuilder:validation:MaxLength=32
	Scheme string `json:"scheme,omitempty"`
}

type Destination struct {
	// Host is a DNS name, or *.<domain> for its subdomains.
	//
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^(\*\.)?([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,}$`
	Host string `json:"host"`

	// +optional
	// +kubebuilder:default=443
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`
}

// +kubebuilder:object:root=true

type CredentialBindingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []CredentialBinding `json:"items"`
}
