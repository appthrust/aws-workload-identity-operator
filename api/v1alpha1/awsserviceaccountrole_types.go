package v1alpha1

import (
	apixv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ServiceAccountSubject identifies a Kubernetes ServiceAccount.
type ServiceAccountSubject struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
}

// AWSServiceAccountRoleSpec defines the desired IAM role and policy delivery.
// +kubebuilder:validation:XValidation:rule="(has(self.policyARNs) && self.policyARNs.size() > 0) || has(self.policyDocument)",message="at least one of spec.policyARNs or spec.policyDocument is required"
type AWSServiceAccountRoleSpec struct {
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.serviceAccount is immutable"
	ServiceAccount ServiceAccountSubject `json:"serviceAccount"`
	// +listType=set
	// +kubebuilder:validation:MaxItems=10
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=2048
	// +kubebuilder:validation:items:Pattern=`^arn:aws[a-z0-9-]*:iam::(aws|[0-9]{12}):policy/[\w+=,.@/-]+$`
	// +optional
	PolicyARNs []string `json:"policyARNs,omitempty"`
	// PolicyDocument is an inline customer-managed IAM policy document attached
	// to the generated role. The top-level shape is bounded so the stored object
	// stays small and constant time to read; IAM polymorphism (Action/Resource/
	// Principal/Condition) is preserved inside each Statement.
	// +optional
	PolicyDocument *AWSIAMPolicyDocument `json:"policyDocument,omitempty"`
}

// AWSIAMPolicyDocument is the bounded structural schema for an inline IAM
// policy document. Bounding every level keeps the stored object size constant
// time per api-conventions.md while preserving IAM polymorphism inside each
// statement.
type AWSIAMPolicyDocument struct {
	// Version is the IAM policy language version. Only the two AWS-recognized
	// values are accepted.
	// +kubebuilder:validation:Enum="2008-10-17";"2012-10-17"
	// +optional
	Version string `json:"Version,omitempty"`

	// ID is an optional identifier for the policy document.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +optional
	ID string `json:"Id,omitempty"`

	// Statement is the bounded list of IAM policy statements. Each statement
	// preserves IAM polymorphic keys (Action/Resource/Principal/Condition) but
	// is capped on top-level key count so a single statement cannot grow
	// unboundedly.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=50
	// +kubebuilder:validation:items:Type=object
	// +kubebuilder:validation:items:XPreserveUnknownFields
	// +kubebuilder:validation:items:MaxProperties=16
	Statement []AWSIAMPolicyStatement `json:"Statement"`
}

// AWSIAMPolicyStatement is one entry in an IAM policy document. Each statement
// preserves IAM-defined polymorphic keys on the wire (Action/Resource/
// Principal/Condition); per-item Type/XPreserveUnknownFields/MaxProperties
// markers on the parent Statement field bound the per-statement breadth.
type AWSIAMPolicyStatement = apixv1.JSON

// AWSServiceAccountRoleStatus reports resolved IAM and delivery state.
type AWSServiceAccountRoleStatus struct {
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
	// +optional
	RoleARN string `json:"roleARN,omitempty"`
	// +optional
	GeneratedPolicyARN string `json:"generatedPolicyARN,omitempty"`
	// DeliveryType records the last resolved delivery strategy used by the
	// role. It is used during deletion if the namespace config was force-deleted.
	// +kubebuilder:validation:Enum=SelfHostedIRSA;EKSPodIdentity;EKSIRSA
	// +optional
	DeliveryType DeliveryType `json:"deliveryType,omitempty"`
	// ResolvedClusterName records the last ready multicluster-runtime cluster
	// identifier for SelfHostedIRSA delivery cleanup.
	// +optional
	ResolvedClusterName string `json:"resolvedClusterName,omitempty"`
}

// AWSServiceAccountRole requests IAM permissions for one Kubernetes ServiceAccount.
// +kubebuilder:printcolumn:name="ServiceAccount",type=string,JSONPath=`.spec.serviceAccount.name`
// +kubebuilder:printcolumn:name="SA-Namespace",type=string,JSONPath=`.spec.serviceAccount.namespace`,priority=1
// +kubebuilder:printcolumn:name="Delivery",type=string,JSONPath=`.status.deliveryType`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`,priority=1
// +kubebuilder:printcolumn:name="RoleARN",type=string,JSONPath=`.status.roleARN`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
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
