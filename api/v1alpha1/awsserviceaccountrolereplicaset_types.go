package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// AWSServiceAccountRoleReplicaSet fans out AWSServiceAccountRole children.
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.status.desiredClusterCount`
// +kubebuilder:printcolumn:name="ReadyClusters",type=integer,JSONPath=`.status.readyClusterCount`
// +kubebuilder:printcolumn:name="Failed",type=integer,JSONPath=`.status.failureCount`
// +kubebuilder:printcolumn:name="Conflicts",type=integer,JSONPath=`.status.conflictCount`,priority=1
// +kubebuilder:printcolumn:name="Stale",type=integer,JSONPath=`.status.staleClusterCount`,priority=1
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=awsarrs
type AWSServiceAccountRoleReplicaSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AWSServiceAccountRoleReplicaSetSpec   `json:"spec,omitempty"`
	Status            AWSServiceAccountRoleReplicaSetStatus `json:"status,omitempty"`
}

// AWSServiceAccountRoleReplicaSetSpec defines fleet-scoped role fan-out.
// +kubebuilder:validation:XValidation:rule="self.template.spec.serviceAccount == oldSelf.template.spec.serviceAccount",message="spec.template.spec.serviceAccount is immutable"
type AWSServiceAccountRoleReplicaSetSpec struct {
	// PlacementRefs references placement outputs in the same namespace as the
	// ReplicaSet. Multiple refs are unioned by cluster identity.
	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=apiGroup
	// +listMapKey=kind
	// +listMapKey=name
	PlacementRefs []PlacementReference `json:"placementRefs"`

	// Template describes the generated AWSServiceAccountRole children.
	Template AWSServiceAccountRoleTemplate `json:"template"`
}

// PlacementReference identifies a same-namespace placement output.
// +kubebuilder:validation:XValidation:rule="self.apiGroup == 'cluster.open-cluster-management.io' && self.kind == 'Placement'",message="apiGroup and kind must identify a supported placement output"
type PlacementReference struct {
	// +kubebuilder:validation:Enum=cluster.open-cluster-management.io
	APIGroup string `json:"apiGroup"`
	// +kubebuilder:validation:Enum=Placement
	Kind string `json:"kind"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
}

// AWSServiceAccountRoleTemplate describes child metadata and spec.
type AWSServiceAccountRoleTemplate struct {
	// Metadata is copied to generated children after reserved metadata is
	// rejected by admission.
	// +optional
	Metadata *TemplateMetadata `json:"metadata,omitempty"`

	// Spec is copied to each generated AWSServiceAccountRole child.
	Spec AWSServiceAccountRoleSpec `json:"spec"`
}

// TemplateMetadata contains labels and annotations copied to children.
type TemplateMetadata struct {
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
}

// AWSServiceAccountRoleReplicaSetStatus reports fleet fan-out state.
type AWSServiceAccountRoleReplicaSetStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	SelectedClusterCount int32 `json:"selectedClusterCount,omitempty"`
	DesiredClusterCount  int32 `json:"desiredClusterCount,omitempty"`
	AppliedClusterCount  int32 `json:"appliedClusterCount,omitempty"`
	ReadyClusterCount    int32 `json:"readyClusterCount,omitempty"`
	StaleClusterCount    int32 `json:"staleClusterCount,omitempty"`
	ConflictCount        int32 `json:"conflictCount,omitempty"`
	FailureCount         int32 `json:"failureCount,omitempty"`

	// +kubebuilder:validation:MaxItems=50
	// +listType=map
	// +listMapKey=clusterName
	FailedClusters []AWSServiceAccountRoleClusterFailure `json:"failedClusters,omitempty"`

	// +kubebuilder:validation:MaxItems=100
	// +listType=map
	// +listMapKey=clusterName
	Clusters []AWSServiceAccountRoleClusterSummary `json:"clusters,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// AWSServiceAccountRoleClusterPhase describes per-cluster fan-out state.
type AWSServiceAccountRoleClusterPhase string

const (
	// AWSServiceAccountRoleClusterPending means the child is not ready yet.
	AWSServiceAccountRoleClusterPending AWSServiceAccountRoleClusterPhase = "Pending"
	// AWSServiceAccountRoleClusterReady means the child reports Ready=True.
	AWSServiceAccountRoleClusterReady AWSServiceAccountRoleClusterPhase = "Ready"
	// AWSServiceAccountRoleClusterConflict means the expected child is foreign.
	AWSServiceAccountRoleClusterConflict AWSServiceAccountRoleClusterPhase = "Conflict"
	// AWSServiceAccountRoleClusterFailed means child apply failed.
	AWSServiceAccountRoleClusterFailed AWSServiceAccountRoleClusterPhase = "Failed"
)

// AWSServiceAccountRoleClusterFailure records a failed cluster fan-out path.
type AWSServiceAccountRoleClusterFailure struct {
	// +kubebuilder:validation:MinLength=1
	ClusterName string `json:"clusterName"`
	// +kubebuilder:validation:Enum=Pending;Ready;Conflict;Failed
	Phase AWSServiceAccountRoleClusterPhase `json:"phase"`
	// +kubebuilder:validation:MaxLength=1024
	Reason string `json:"reason,omitempty"`
	// +kubebuilder:validation:MaxLength=32768
	Message string `json:"message,omitempty"`
}

// AWSServiceAccountRoleClusterSummary records the child role state per cluster.
type AWSServiceAccountRoleClusterSummary struct {
	// +kubebuilder:validation:MinLength=1
	ClusterName string `json:"clusterName"`
	Namespace   string `json:"namespace,omitempty"`
	Name        string `json:"name,omitempty"`
	// +kubebuilder:validation:Enum=Pending;Ready;Conflict;Failed
	Phase AWSServiceAccountRoleClusterPhase `json:"phase"`
	Ready bool                              `json:"ready,omitempty"`
	// +kubebuilder:validation:MaxLength=1024
	Reason string `json:"reason,omitempty"`
	// +kubebuilder:validation:MaxLength=32768
	Message string `json:"message,omitempty"`
}

// AWSServiceAccountRoleReplicaSetList contains AWSServiceAccountRoleReplicaSet objects.
// +kubebuilder:object:root=true
type AWSServiceAccountRoleReplicaSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AWSServiceAccountRoleReplicaSet `json:"items"`
}
