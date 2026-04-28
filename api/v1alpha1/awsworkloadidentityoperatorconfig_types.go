package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// SelfHostedIRSAConfig configures self-hosted IRSA runtime delivery.
type SelfHostedIRSAConfig struct {
	// +kubebuilder:default=aws-pod-identity-webhook
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.selfHostedIRSA.webhookNamespace is immutable"
	// +optional
	WebhookNamespace string `json:"webhookNamespace,omitempty"`
}

// AWSWorkloadIdentityOperatorConfigSpec defines cluster-wide operator defaults.
type AWSWorkloadIdentityOperatorConfigSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^arn:aws[a-z0-9-]*:iam::(aws|[0-9]{12}):policy/[\w+=,.@/-]+$`
	// +optional
	PermissionsBoundaryARN string `json:"permissionsBoundaryARN,omitempty"`
	// +optional
	SelfHostedIRSA SelfHostedIRSAConfig `json:"selfHostedIRSA,omitempty"`
}

// AWSWorkloadIdentityOperatorConfig configures cluster-wide operator behavior.
// +kubebuilder:printcolumn:name="Webhook-Namespace",type=string,JSONPath=`.spec.selfHostedIRSA.webhookNamespace`
// +kubebuilder:printcolumn:name="Permissions-Boundary",type=string,JSONPath=`.spec.permissionsBoundaryARN`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster,shortName=awioc
// +kubebuilder:validation:XValidation:rule="self.metadata.name == 'default'",message="metadata.name must be default"
type AWSWorkloadIdentityOperatorConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AWSWorkloadIdentityOperatorConfigSpec `json:"spec,omitempty"`
}

// AWSWorkloadIdentityOperatorConfigList contains AWSWorkloadIdentityOperatorConfig objects.
// +kubebuilder:object:root=true
type AWSWorkloadIdentityOperatorConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AWSWorkloadIdentityOperatorConfig `json:"items"`
}
