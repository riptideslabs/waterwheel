// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CredentialSource says how to exchange a workload's JWT-SVID for a provider's
// token. Field names follow the platform's CredentialSource.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=credsrc
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type CredentialSource struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CredentialSourceSpec `json:"spec"`
	Status Status               `json:"status,omitempty"`
}

// CredentialSourceSpec holds exactly one provider.
//
// +kubebuilder:validation:XValidation:rule="[has(self.anthropic), has(self.openai), has(self.gcp), has(self.azure), has(self.rfc7523), has(self.rfc8693)].filter(x, x).size() == 1",message="set exactly one provider"
type CredentialSourceSpec struct {
	// +optional
	Anthropic *AnthropicWIFCredentialProvider `json:"anthropic,omitempty"`
	// +optional
	OpenAI *OpenAIWIFCredentialProvider `json:"openai,omitempty"`
	// +optional
	GCP *GCPWIFCredentialProvider `json:"gcp,omitempty"`
	// +optional
	Azure *EntraWIFCredentialProvider `json:"azure,omitempty"`
	// +optional
	RFC7523 *RFC7523CredentialProvider `json:"rfc7523,omitempty"`
	// +optional
	RFC8693 *RFC8693CredentialProvider `json:"rfc8693,omitempty"`
}

// AnthropicWIFCredentialProvider exchanges the SVID at Anthropic's workload
// identity federation endpoint (RFC 7523).
type AnthropicWIFCredentialProvider struct {
	// +kubebuilder:validation:Format=uuid
	OrganizationID string `json:"organizationId"`
	// +kubebuilder:validation:Pattern=`^(wrkspc_[a-zA-Z0-9]+|default)$`
	WorkspaceID string `json:"workspaceId"`
	// +kubebuilder:validation:Pattern=`^svac_[a-zA-Z0-9]+$`
	ServiceAccountID string `json:"serviceAccountId"`
	// +kubebuilder:validation:Pattern=`^fdrl_[a-zA-Z0-9]+$`
	FederationRuleID string `json:"federationRuleId"`

	// IDTokenAudience is the JWT-SVID's audience. Defaults to https://api.anthropic.com.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=8
	IDTokenAudience []string `json:"idTokenAudience,omitempty"`
}

// OpenAIWIFCredentialProvider exchanges the SVID at OpenAI's workload identity
// federation endpoint (RFC 8693).
type OpenAIWIFCredentialProvider struct {
	// +kubebuilder:validation:MinLength=1
	IdentityProviderID string `json:"identityProviderId"`
	// +kubebuilder:validation:MinLength=1
	ServiceAccountID string `json:"serviceAccountId"`
	// +optional
	ClientID string `json:"clientId,omitempty"`

	// IDTokenAudience is the JWT-SVID's audience. Defaults to https://api.openai.com.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=8
	IDTokenAudience []string `json:"idTokenAudience,omitempty"`
}

// GCPWIFCredentialProvider exchanges the SVID at Google's Security Token
// Service through a workload identity pool provider, and can then
// impersonate a service account.
//
// +kubebuilder:validation:XValidation:rule="!has(self.lifetime) || has(self.serviceAccount)",message="lifetime needs serviceAccount"
type GCPWIFCredentialProvider struct {
	// OIDCProviderID is the provider's full resource name, which is also the
	// STS audience.
	//
	// +kubebuilder:validation:Pattern=`^//iam\.googleapis\.com/projects/[0-9]{6,12}/locations/global/workloadIdentityPools/[a-z][a-z0-9-]{2,30}[a-z0-9]/providers/[a-z][a-z0-9-]{2,30}[a-z0-9]$`
	OIDCProviderID string `json:"oidcProviderId"`

	// ServiceAccount to impersonate. Without it, the token belongs to the
	// federated identity itself.
	//
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]{4,28}[a-z0-9]@[a-z][a-z0-9-]{4,28}[a-z0-9]\.iam\.gserviceaccount\.com$`
	ServiceAccount string `json:"serviceAccount,omitempty"`

	// Scopes default to cloud-platform and userinfo.email.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:Pattern=`^https://www\.googleapis\.com/auth/[a-zA-Z0-9._-]+$`
	Scopes []string `json:"scopes,omitempty"`

	// Lifetime of the impersonated service account's token. Defaults to 1h.
	//
	// +optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=16
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1m') && duration(self) <= duration('1h')",message="must be between 1m and 1h"
	Lifetime *metav1.Duration `json:"lifetime,omitempty"`

	// Audience is the JWT-SVID's audience. Defaults to https: followed by
	// oidcProviderId, which is the provider's default allowed audience.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=8
	Audience []string `json:"audience,omitempty"`
}

// EntraWIFCredentialProvider exchanges the SVID for a Microsoft Entra ID
// access token. The SVID is the client assertion of an app registration or
// managed identity that has a federated identity credential for it.
type EntraWIFCredentialProvider struct {
	// +kubebuilder:validation:Format=uuid
	ClientID string `json:"clientId"`
	// +kubebuilder:validation:Format=uuid
	TenantID string `json:"tenantId"`

	// Scope of the access token, e.g. https://cognitiveservices.azure.com/.default.
	//
	// +kubebuilder:validation:Pattern=`^https://[a-z0-9.-]+/\.default$`
	Scope string `json:"scope"`

	// Audience is the JWT-SVID's audience. Defaults to api://AzureADTokenExchange.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=8
	Audience []string `json:"audience,omitempty"`
}

// RFC7523CredentialProvider presents the SVID as a JWT bearer grant.
type RFC7523CredentialProvider struct {
	// +kubebuilder:validation:Pattern=`^https://`
	TokenEndpointURL string `json:"tokenEndpointUrl"`

	// +optional
	// +kubebuilder:validation:Enum=BODY_FORMAT_FORM;BODY_FORMAT_JSON
	BodyFormat string `json:"bodyFormat,omitempty"`

	// +optional
	Scopes []string `json:"scopes,omitempty"`

	// +optional
	AdditionalParams []AdditionalParam `json:"additionalParams,omitempty"`

	// IDTokenAudience is the JWT-SVID's audience. Defaults to the token endpoint URL.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=8
	IDTokenAudience []string `json:"idTokenAudience,omitempty"`
}

type AdditionalParam struct {
	Key string `json:"key"`
	// +optional
	Values []string `json:"values,omitempty"`
}

// RFC8693CredentialProvider presents the SVID as the subject token of an
// OAuth 2.0 token exchange.
type RFC8693CredentialProvider struct {
	// +kubebuilder:validation:Pattern=`^https://`
	TokenEndpointURL string `json:"tokenEndpointUrl"`

	// +optional
	// +kubebuilder:validation:Enum=SUBJECT_TOKEN_TYPE_JWT;SUBJECT_TOKEN_TYPE_ID_TOKEN
	SubjectTokenType string `json:"subjectTokenType,omitempty"`

	// +optional
	RequestedTokenType string `json:"requestedTokenType,omitempty"`

	// Audience is the token exchange's audience parameter, not the SVID's.
	//
	// +optional
	Audience string `json:"audience,omitempty"`

	// +optional
	Scopes []string `json:"scopes,omitempty"`

	// +optional
	AdditionalFields map[string]string `json:"additionalFields,omitempty"`

	// IDTokenAudience is the JWT-SVID's audience. Defaults to the token endpoint URL.
	//
	// +optional
	// +kubebuilder:validation:MaxItems=8
	IDTokenAudience []string `json:"idTokenAudience,omitempty"`
}

// +kubebuilder:object:root=true

type CredentialSourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []CredentialSource `json:"items"`
}
