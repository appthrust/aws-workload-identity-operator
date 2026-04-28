package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
// +kubebuilder:validation:XValidation:rule="(has(self.policyARNs) && self.policyARNs.size() > 0) != has(self.policyDocument)",message="exactly one of spec.policyARNs or spec.policyDocument must be specified"
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
	// to the generated role. The controller serializes the object to compact JSON
	// before passing it to AWS IAM through ACK.
	//
	// Keep this field as an object, not a string. Kubernetes admission should
	// validate that authors supplied a JSON/YAML object, and the operator should
	// own the final compact JSON serialization at the AWS API boundary. Modeling
	// this as a string would make YAML literal whitespace and invalid JSON shape
	// user-visible failure modes again.
	// +optional
	// +kubebuilder:validation:MinProperties=1
	// +kubebuilder:validation:Type=object
	// +kubebuilder:pruning:PreserveUnknownFields
	PolicyDocument *runtime.RawExtension `json:"policyDocument,omitempty"`
}

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
	// RoleARN is the IAM Role ARN generated for the bound ServiceAccount.
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^arn:aws[a-z0-9-]*:iam::[0-9]{12}:role/[\w+=,.@/-]+$`
	RoleARN string `json:"roleARN,omitempty"`
	// GeneratedPolicyARN is the customer-managed IAM Policy ARN generated
	// when `spec.policyDocument` is set. The shape mirrors the items pattern
	// on `spec.policyARNs`.
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^arn:aws[a-z0-9-]*:iam::(aws|[0-9]{12}):policy/[\w+=,.@/-]+$`
	GeneratedPolicyARN string `json:"generatedPolicyARN,omitempty"`
	// DeliveryType records the last resolved delivery strategy used by the
	// role. It is used during deletion if the namespace config was force-deleted.
	// +kubebuilder:validation:Enum=SelfHostedIRSA;EKSPodIdentity;EKSIRSA
	// +optional
	DeliveryType DeliveryType `json:"deliveryType,omitempty"`
	// ResolvedClusterName records the last ready multicluster-runtime cluster
	// identifier for SelfHostedIRSA delivery cleanup. The controller encodes
	// the identifier as `<namespace>/<name>` via
	// `types.NamespacedName.String()`, where both segments are DNS-1123
	// subdomains (and are currently equal — see `logicalClusterName` in
	// `internal/inventory/resolver.go`). The bound covers two maximum-length
	// subdomain segments and the separating slash (253 + 1 + 253).
	// +optional
	// +kubebuilder:validation:MaxLength=507
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
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
	Spec              AWSServiceAccountRoleSpec   `json:"spec"`
	Status            AWSServiceAccountRoleStatus `json:"status,omitempty"`
}

// AWSServiceAccountRoleList contains AWSServiceAccountRole objects.
// +kubebuilder:object:root=true
type AWSServiceAccountRoleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AWSServiceAccountRole `json:"items"`
}
