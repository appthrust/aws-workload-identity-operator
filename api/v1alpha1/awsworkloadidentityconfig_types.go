package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AWSWorkloadIdentityConfigSpec defines namespace-scoped workload identity delivery.
// +kubebuilder:validation:XValidation:rule="has(self.eksIRSA) == (self.type == 'EKSIRSA')",message="spec.eksIRSA must be present exactly when spec.type is EKSIRSA"
type AWSWorkloadIdentityConfigSpec struct {
	// +kubebuilder:validation:Enum=SelfHostedIRSA;EKSPodIdentity;EKSIRSA
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.type is immutable"
	Type DeliveryType `json:"type"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[a-z]{2,}-[a-z0-9-]+-[0-9]+$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.region is immutable"
	Region string `json:"region"`

	// EKSIRSA configures native EKS IRSA delivery. It must be present only when
	// spec.type is EKSIRSA.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.eksIRSA is immutable"
	EKSIRSA *EKSIRSAConfig `json:"eksIRSA,omitempty"`
}

// EKSIRSAConfig configures native EKS OIDC issuer based IRSA delivery.
type EKSIRSAConfig struct {
	// IssuerURL is the canonical EKS OIDC issuer URL.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^https://oidc\.eks\.[a-z]{2,}-[a-z0-9-]+-[0-9]+\.[a-z0-9.-]+/id/[A-Z0-9]+$`
	IssuerURL string `json:"issuerURL"`

	// OIDCProvider selects whether the operator manages the IAM OIDC provider
	// ACK resource or uses an externally managed provider ARN.
	OIDCProvider EKSIRSAOIDCProviderConfig `json:"oidcProvider"`
}

// EKSIRSAOIDCProviderConfig configures IAM OIDC provider ownership for EKSIRSA.
// +kubebuilder:validation:XValidation:rule="self.management == 'Managed' ? !has(self.arn) : has(self.arn)",message="spec.eksIRSA.oidcProvider.arn must be omitted for Managed and required for External"
type EKSIRSAOIDCProviderConfig struct {
	// +kubebuilder:validation:Enum=Managed;External
	Management OIDCProviderManagement `json:"management"`

	// ARN is required when management is External and forbidden when management is Managed.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^arn:aws[a-z0-9-]*:iam::[0-9]{12}:oidc-provider/[A-Za-z0-9._~%!$&'()*+,;=:@/-]+$`
	ARN string `json:"arn,omitempty"`
}

// AWSWorkloadIdentityConfigStatus reports delivery and AWS resource state.
type AWSWorkloadIdentityConfigStatus struct {
	// +kubebuilder:validation:Minimum=0
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=32
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
	// +listType=map
	// +listMapKey=apiVersion
	// +listMapKey=kind
	// +listMapKey=namespace
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=16
	// +optional
	ACKResources []ACKResourceStatus `json:"ackResources,omitempty"`

	// SelfHostedIssuer reports the current self-hosted issuer target and the
	// last S3 publication identity verified by the controller.
	// +optional
	SelfHostedIssuer *AWSWorkloadIdentityConfigSelfHostedIssuerStatus `json:"selfHostedIssuer,omitempty"`

	// +optional
	IssuerHostPath string `json:"issuerHostPath,omitempty"`
	// +optional
	OIDCProviderARN string `json:"oidcProviderARN,omitempty"`

	// ResolvedClusterName records the multicluster-runtime cluster identifier
	// used by the latest ready self-hosted inventory resolution.
	// +optional
	ResolvedClusterName string `json:"resolvedClusterName,omitempty"`

	// +optional
	WebhookRuntimeNamespace string `json:"webhookRuntimeNamespace,omitempty"`
	// +optional
	WebhookRuntimeCertNotAfter *metav1.Time `json:"webhookRuntimeCertNotAfter,omitempty"`
}

// AWSWorkloadIdentityConfigSelfHostedIssuerStatus reports the desired
// self-hosted issuer bucket and the last verified object publication.
type AWSWorkloadIdentityConfigSelfHostedIssuerStatus struct {
	// BucketName is the current desired self-hosted issuer bucket target.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	BucketName string `json:"bucketName"`

	// Publication is the current publication verified in S3. It is nil until
	// the controller confirms the issuer objects are current.
	// +optional
	Publication *AWSWorkloadIdentityConfigSelfHostedIssuerPublicationStatus `json:"publication,omitempty"`
}

// AWSWorkloadIdentityConfigSelfHostedIssuerPublicationStatus identifies the
// self-hosted issuer object set last verified in S3.
type AWSWorkloadIdentityConfigSelfHostedIssuerPublicationStatus struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	BucketName string `json:"bucketName"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https://.+$`
	IssuerURL string `json:"issuerURL"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_-]+$`
	SigningKeyID string `json:"signingKeyID"`

	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	ObjectSetDigest string `json:"objectSetDigest"`
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
