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
	Spec              AWSServiceAccountRoleReplicaSetSpec   `json:"spec"`
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
//
// The XValidation rules cap the embedded OCM RolloutStrategy's
// mandatoryDecisionGroups list size. A per-element groupName length cap is
// intentionally absent: the upstream OCM type declares no maxItems on
// mandatoryDecisionGroups, so any CEL .all() macro over the list trips the
// apiserver's static cost analyzer (it assumes math.MaxInt32 cardinality when
// maxItems is missing).
// +kubebuilder:validation:XValidation:rule="!has(self.rolloutStrategy.progressive) || !has(self.rolloutStrategy.progressive.mandatoryDecisionGroups) || size(self.rolloutStrategy.progressive.mandatoryDecisionGroups) <= 32",message="rolloutStrategy.progressive.mandatoryDecisionGroups must have at most 32 entries"
// +kubebuilder:validation:XValidation:rule="!has(self.rolloutStrategy.progressivePerGroup) || !has(self.rolloutStrategy.progressivePerGroup.mandatoryDecisionGroups) || size(self.rolloutStrategy.progressivePerGroup.mandatoryDecisionGroups) <= 32",message="rolloutStrategy.progressivePerGroup.mandatoryDecisionGroups must have at most 32 entries"
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
	// +kubebuilder:validation:MaxProperties=64
	// +kubebuilder:validation:XValidation:rule="self.all(k, size(k) <= 317 && k.matches('^([a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?/)?[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$'))",message="label keys must be valid Kubernetes qualified names"
	// +kubebuilder:validation:XValidation:rule="self.all(k, !(k in ['app.kubernetes.io/managed-by','aws.identity.appthrust.io/binding-uid','aws.identity.appthrust.io/config-uid','aws.identity.appthrust.io/delivery','aws.identity.appthrust.io/inventory-namespace','aws.identity.appthrust.io/owner-ref','aws.identity.appthrust.io/replicaset-uid','aws.identity.appthrust.io/runtime','aws.identity.appthrust.io/service-account']))",message="label key is reserved by aws-workload-identity-operator; reserved keys: app.kubernetes.io/managed-by, aws.identity.appthrust.io/{binding-uid,config-uid,delivery,inventory-namespace,owner-ref,replicaset-uid,runtime,service-account}"
	Labels map[string]KubernetesLabelValue `json:"labels,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxProperties=64
	// +kubebuilder:validation:XValidation:rule="self.all(k, size(k) <= 317 && k.matches('^([a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?/)?[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$'))",message="annotation keys must be valid Kubernetes qualified names"
	// +kubebuilder:validation:XValidation:rule="self.all(k, !(k in ['aws.identity.appthrust.io/replicaset-owner-ref']))",message="annotation key 'aws.identity.appthrust.io/replicaset-owner-ref' is reserved by aws-workload-identity-operator"
	// +kubebuilder:validation:XValidation:rule="self.map(k, size(k) + size(self[k])).sum() <= 261120",message="annotations total size (sum of all key and value bytes) must not exceed 261120 bytes; the remaining 1024 bytes of apimachinery's 262144-byte TotalAnnotationSizeLimitB are reserved for operator-stamped annotations"
	Annotations map[string]KubernetesAnnotationValue `json:"annotations,omitempty"`
}

// KubernetesLabelValue is a Kubernetes label value. The alias keeps the Go API
// ergonomic while giving controller-gen a schema for map values.
// +kubebuilder:validation:MaxLength=63
// +kubebuilder:validation:Pattern=`^(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])?$`
type KubernetesLabelValue = string

// KubernetesAnnotationValue is a Kubernetes annotation value. controller-gen
// does not project field-level markers onto map additionalProperties, so the
// per-value bound is expressed via an alias mirroring KubernetesLabelValue.
// The per-value cap matches apimachinery's TotalAnnotationSizeLimitB (256 KiB)
// so the schema rejects any single payload that could not fit in the
// apiserver's aggregate annotation budget. Aggregate enforcement (sum of all
// key and value bytes) lives on the map itself as an x-kubernetes-validations
// rule on TemplateMetadata.Annotations.
// +kubebuilder:validation:MaxLength=262144
type KubernetesAnnotationValue = string

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
	// +kubebuilder:validation:MaxItems=32
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
	// Name mirrors the same-namespace OCM Placement name; the bound matches
	// `spec.placementRefs[*].name`.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`

	// +kubebuilder:validation:Minimum=0
	// +optional
	SelectedClusterCount int32 `json:"selectedClusterCount,omitempty"`

	// AvailableDecisionGroups is a free-form OCM rollout summary copied from
	// the placement (e.g. "3 (5 / 10 clusters applied)"). It is bounded so a
	// single status object cannot grow unboundedly.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	AvailableDecisionGroups string `json:"availableDecisionGroups,omitempty"`

	// +optional
	Rollout *AWSServiceAccountRoleRolloutSummary `json:"rollout,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +kubebuilder:validation:MaxItems=32
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
	// +kubebuilder:validation:MaxItems=32
	// +patchStrategy=merge
	// +patchMergeKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// AWSServiceAccountRoleClusterFailure records a failed cluster fan-out path.
type AWSServiceAccountRoleClusterFailure struct {
	// ClusterName is an OCM ManagedCluster identifier (DNS-1123 subdomain).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
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
	// ClusterName is an OCM ManagedCluster identifier (DNS-1123 subdomain).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	ClusterName string `json:"clusterName"`
	// Namespace is the hub-side namespace assigned to this cluster's child
	// AWSServiceAccountRole. The controller writes the OCM ManagedCluster
	// name (DNS-1123 subdomain) here verbatim — see
	// `existingReplicaSetClusterSummary` and `summaryFromRolloutStatus` in
	// `internal/controller/role_replicaset_controller.go`.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Namespace string `json:"namespace,omitempty"`
	// Name is the child AWSServiceAccountRole name on the workload cluster
	// (DNS-1123 subdomain).
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
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
