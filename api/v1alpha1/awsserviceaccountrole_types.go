package v1alpha1

import (
	apixv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ServiceAccountSubject identifies a Kubernetes ServiceAccount.
type ServiceAccountSubject struct {
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// AWSServiceAccountRoleSpec defines the desired IAM role and policy delivery.
// +kubebuilder:validation:XValidation:rule="(has(self.policyARNs) && self.policyARNs.size() > 0) || has(self.policyDocument)",message="at least one of spec.policyARNs or spec.policyDocument is required"
type AWSServiceAccountRoleSpec struct {
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.serviceAccount is immutable"
	ServiceAccount ServiceAccountSubject `json:"serviceAccount"`
	PolicyARNs     []string              `json:"policyARNs,omitempty"`
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Type=object
	PolicyDocument *apixv1.JSON `json:"policyDocument,omitempty"`
}

// AWSServiceAccountRoleStatus reports resolved IAM and delivery state.
type AWSServiceAccountRoleStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +listType=map
	// +listMapKey=apiVersion
	// +listMapKey=kind
	// +listMapKey=namespace
	// +listMapKey=name
	ACKResources       []ACKResourceStatus `json:"ackResources,omitempty"`
	RoleARN            string              `json:"roleARN,omitempty"`
	GeneratedPolicyARN string              `json:"generatedPolicyARN,omitempty"`
	// DeliveryType records the last resolved delivery strategy used by the
	// role. It is used during deletion if the namespace config was force-deleted.
	// +kubebuilder:validation:Enum=SelfHostedIRSA;EKSPodIdentity
	DeliveryType DeliveryType `json:"deliveryType,omitempty"`
	// ResolvedClusterName records the last ready multicluster-runtime cluster
	// identifier for SelfHostedIRSA delivery cleanup.
	ResolvedClusterName string `json:"resolvedClusterName,omitempty"`
}

// AWSServiceAccountRole requests IAM permissions for one Kubernetes ServiceAccount.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=awsar
type AWSServiceAccountRole struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AWSServiceAccountRoleSpec   `json:"spec,omitempty"`
	Status            AWSServiceAccountRoleStatus `json:"status,omitempty"`
}

// AWSServiceAccountRoleList contains AWSServiceAccountRole objects.
// +kubebuilder:object:root=true
type AWSServiceAccountRoleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AWSServiceAccountRole `json:"items"`
}
