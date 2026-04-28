package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1alpha1 "open-cluster-management.io/api/cluster/v1alpha1"
)

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
	// PlacementRefs references same-namespace OCM Placement objects. Multiple
	// refs are unioned by cluster identity.
	// +required
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	PlacementRefs []PlacementRef `json:"placementRefs"`

	// Template describes the generated AWSServiceAccountRole children.
	Template AWSServiceAccountRoleTemplate `json:"template"`
}

// PlacementRef identifies a same-namespace OCM Placement.
type PlacementRef struct {
	// Name is the name of the OCM Placement in the same namespace.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`

	// RolloutStrategy controls how generated AWSServiceAccountRole children
	// are applied across clusters selected by this placement ref.
	// +kubebuilder:default={type: All}
	// +optional
	RolloutStrategy clusterv1alpha1.RolloutStrategy `json:"rolloutStrategy,omitempty"`
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
	// +kubebuilder:validation:Minimum=0
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	SelectedClusterCount int32 `json:"selectedClusterCount,omitempty"`
	// +optional
	DesiredClusterCount int32 `json:"desiredClusterCount,omitempty"`
	// +optional
	AppliedClusterCount int32 `json:"appliedClusterCount,omitempty"`
	// +optional
	ReadyClusterCount int32 `json:"readyClusterCount,omitempty"`
	// +optional
	StaleClusterCount int32 `json:"staleClusterCount,omitempty"`
	// +optional
	ConflictCount int32 `json:"conflictCount,omitempty"`
	// +optional
	FailureCount int32 `json:"failureCount,omitempty"`

	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=name
	// +optional
	Placements []AWSServiceAccountRolePlacementStatus `json:"placements,omitempty"`

	// +optional
	Rollout *AWSServiceAccountRoleRolloutSummary `json:"rollout,omitempty"`

	// +kubebuilder:validation:MaxItems=50
	// +listType=map
	// +listMapKey=clusterName
	// +optional
	FailedClusters []AWSServiceAccountRoleClusterFailure `json:"failedClusters,omitempty"`

	// +kubebuilder:validation:MaxItems=100
	// +listType=map
	// +listMapKey=clusterName
	// +optional
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
	// AWSServiceAccountRoleClusterTimedOut means rollout timed out before the
	// child reached the desired state.
	AWSServiceAccountRoleClusterTimedOut AWSServiceAccountRoleClusterPhase = "TimedOut"
)

// AWSServiceAccountRolePlacementStatus reports one placement ref resolution
// and rollout state.
type AWSServiceAccountRolePlacementStatus struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// +kubebuilder:validation:Minimum=0
	// +optional
	SelectedClusterCount int32 `json:"selectedClusterCount,omitempty"`

	// +optional
	AvailableDecisionGroups string `json:"availableDecisionGroups,omitempty"`

	// +optional
	Rollout *AWSServiceAccountRoleRolloutSummary `json:"rollout,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// AWSServiceAccountRoleRolloutSummary reports OCM rollout progress counts.
type AWSServiceAccountRoleRolloutSummary struct {
	// +kubebuilder:validation:Minimum=0
	// +optional
	Total int32 `json:"total,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	Updating int32 `json:"updating,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	Succeeded int32 `json:"succeeded,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	Failed int32 `json:"failed,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +optional
	TimedOut int32 `json:"timedOut,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// AWSServiceAccountRoleClusterFailure records a failed cluster fan-out path.
type AWSServiceAccountRoleClusterFailure struct {
	// +kubebuilder:validation:MinLength=1
	ClusterName string `json:"clusterName"`
	// +kubebuilder:validation:Enum=Pending;Ready;Conflict;Failed;TimedOut
	Phase AWSServiceAccountRoleClusterPhase `json:"phase"`
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Reason string `json:"reason,omitempty"`
	// +kubebuilder:validation:MaxLength=32768
	// +optional
	Message string `json:"message,omitempty"`
}

// AWSServiceAccountRoleClusterSummary records the child role state per cluster.
type AWSServiceAccountRoleClusterSummary struct {
	// +kubebuilder:validation:MinLength=1
	ClusterName string `json:"clusterName"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// +optional
	Name string `json:"name,omitempty"`
	// +kubebuilder:validation:Enum=Pending;Ready;Conflict;Failed;TimedOut
	Phase AWSServiceAccountRoleClusterPhase `json:"phase"`
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	Reason string `json:"reason,omitempty"`
	// +kubebuilder:validation:MaxLength=32768
	// +optional
	Message string `json:"message,omitempty"`
}

// AWSServiceAccountRoleReplicaSetList contains AWSServiceAccountRoleReplicaSet objects.
// +kubebuilder:object:root=true
type AWSServiceAccountRoleReplicaSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AWSServiceAccountRoleReplicaSet `json:"items"`
}
