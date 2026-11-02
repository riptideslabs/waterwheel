// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 is Waterwheel's API: WorkloadIdentity, CredentialSource and
// CredentialBinding. It is modeled on the Riptides platform's core.riptides.io
// API and keeps its group, kinds and field names where they overlap.
//
// +kubebuilder:object:generate=true
// +groupName=core.riptides.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "core.riptides.io", Version: "v1alpha1"}
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion,
		&WorkloadIdentity{}, &WorkloadIdentityList{},
		&CredentialSource{}, &CredentialSourceList{},
		&CredentialBinding{}, &CredentialBindingList{},
	)
	metav1.AddToGroupVersion(s, GroupVersion)

	return nil
}
