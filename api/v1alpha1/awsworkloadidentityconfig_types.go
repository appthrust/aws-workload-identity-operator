package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AWSWorkloadIdentityConfigSpec defines namespace-scoped workload identity delivery.
type AWSWorkloadIdentityConfigSpec struct {
	// +kubebuilder:validation:Enum=SelfHostedIRSA;EKSPodIdentity
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.type is immutable"
	Type DeliveryType `json:"type"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.region is immutable"
	Region string `json:"region"`
}

// AWSWorkloadIdentityConfigStatus reports delivery and AWS resource state.
type AWSWorkloadIdentityConfigStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
	// +listType=map
	// +listMapKey=apiVersion
	// +listMapKey=kind
	// +listMapKey=namespace
	// +listMapKey=name
	ACKResources    []ACKResourceStatus `json:"ackResources,omitempty"`
	BucketName      string              `json:"bucketName,omitempty"`
	IssuerHostPath  string              `json:"issuerHostPath,omitempty"`
	OIDCProviderARN string              `json:"oidcProviderARN,omitempty"`

	// PublishedKeyID is the signing key ID published to the self-hosted issuer
	// objects in S3. When this matches the current signing Secret key ID, the
	// controller skips re-uploading discovery and JWKS objects.
	PublishedKeyID string `json:"publishedKeyID,omitempty"`

	// ResolvedClusterName records the multicluster-runtime cluster identifier
	// used by the latest ready self-hosted inventory resolution.
	ResolvedClusterName string `json:"resolvedClusterName,omitempty"`

	WebhookRuntimeNamespace    string       `json:"webhookRuntimeNamespace,omitempty"`
	WebhookRuntimeCertNotAfter *metav1.Time `json:"webhookRuntimeCertNotAfter,omitempty"`
}

// IssuerURL returns the public HTTPS URL of the self-hosted OIDC issuer derived
// from the issuer host path. Empty if the host path is not yet set.
func (s *AWSWorkloadIdentityConfigStatus) IssuerURL() string {
	if s.IssuerHostPath == "" {
		return ""
	}

	return "https://" + s.IssuerHostPath
}

// AWSWorkloadIdentityConfig configures namespace-scoped workload identity delivery.
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Region",type=string,JSONPath=`.spec.region`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`,priority=1
// +kubebuilder:printcolumn:name="Issuer",type=string,JSONPath=`.status.issuerHostPath`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=awic
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'default'",message="metadata.name must be default"
type AWSWorkloadIdentityConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AWSWorkloadIdentityConfigSpec   `json:"spec,omitempty"`
	Status            AWSWorkloadIdentityConfigStatus `json:"status,omitempty"`
}

// IsForceDelete reports whether the config opts in to bypassing
// AWSServiceAccountRole-based deletion blocking.
func (c *AWSWorkloadIdentityConfig) IsForceDelete() bool {
	return c.Annotations[ForceDeleteAnnotation] == "true"
}

// AWSWorkloadIdentityConfigList contains AWSWorkloadIdentityConfig objects.
// +kubebuilder:object:root=true
type AWSWorkloadIdentityConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AWSWorkloadIdentityConfig `json:"items"`
}
