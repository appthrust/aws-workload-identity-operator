package controller

import (
	"sigs.k8s.io/controller-runtime/pkg/client"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
)

// IndexRoleByServiceAccount is the field index key for AWSServiceAccountRole
// keyed by spec.serviceAccount. Index value: "<sa.namespace>/<sa.name>".
//
// Invariant: callers MUST scope queries with client.InNamespace(inventoryNs)
// because the index value does not encode the role's own namespace. This is
// safe because the operator's inventory model maps each cluster to exactly one
// inventory namespace via inventoryNamespaceFromCluster.
const IndexRoleByServiceAccount = "spec.serviceAccount"

// IndexRoleByReplicaSetUID is the field index key for AWSServiceAccountRole
// children keyed by the owning AWSServiceAccountRoleReplicaSet UID label.
const IndexRoleByReplicaSetUID = "metadata.labels.aws\\.identity\\.appthrust\\.io/replicaset-uid"

// IndexConfigByResolvedCluster is the field index key for
// AWSWorkloadIdentityConfig keyed by status.resolvedClusterName.
const IndexConfigByResolvedCluster = "status.resolvedClusterName"

// IndexReplicaSetByPlacementRef is the field index key for
// AWSServiceAccountRoleReplicaSet keyed by the names referenced in
// spec.placementRefs. Watch handlers for OCM Placement / PlacementDecision
// use this index to enqueue only the ReplicaSets that actually reference the
// changed Placement, avoiding a namespace-wide LIST per watch event.
const IndexReplicaSetByPlacementRef = "spec.placementRefs.name"

// IndexAWSServiceAccountRoleBySA extracts the ServiceAccount lookup key used
// by IndexRoleByServiceAccount.
func IndexAWSServiceAccountRoleBySA(obj client.Object) []string {
	role, ok := obj.(*identityv1.AWSServiceAccountRole)
	if !ok {
		return nil
	}

	if role.Spec.ServiceAccount.Namespace == "" || role.Spec.ServiceAccount.Name == "" {
		return nil
	}

	return []string{serviceAccountIndexKey(role.Spec.ServiceAccount.Namespace, role.Spec.ServiceAccount.Name)}
}

// IndexAWSServiceAccountRoleByReplicaSetUID extracts the ReplicaSet UID label.
func IndexAWSServiceAccountRoleByReplicaSetUID(obj client.Object) []string {
	role, ok := obj.(*identityv1.AWSServiceAccountRole)
	if !ok {
		return nil
	}

	uid := role.GetLabels()[identityv1.LabelReplicaSetUID]
	if uid == "" {
		return nil
	}

	return []string{uid}
}

// IndexAWSServiceAccountRoleReplicaSetByPlacementRef extracts every placement
// reference name from a ReplicaSet's spec.placementRefs. A ReplicaSet that
// references N Placements produces N index entries, one per placement name.
func IndexAWSServiceAccountRoleReplicaSetByPlacementRef(obj client.Object) []string {
	rs, ok := obj.(*identityv1.AWSServiceAccountRoleReplicaSet)
	if !ok {
		return nil
	}

	if len(rs.Spec.PlacementRefs) == 0 {
		return nil
	}

	names := make([]string, 0, len(rs.Spec.PlacementRefs))
	for _, ref := range rs.Spec.PlacementRefs {
		if ref.Name == "" {
			continue
		}

		names = append(names, ref.Name)
	}

	return names
}

// IndexAWSWorkloadIdentityConfigByResolvedCluster extracts the resolved
// multicluster-runtime cluster identifier for self-hosted configs.
func IndexAWSWorkloadIdentityConfigByResolvedCluster(obj client.Object) []string {
	config, ok := obj.(*identityv1.AWSWorkloadIdentityConfig)
	if !ok {
		return nil
	}

	if config.Spec.Type != identityv1.DeliveryTypeSelfHostedIRSA || config.Status.ResolvedClusterName == "" {
		return nil
	}

	return []string{config.Status.ResolvedClusterName}
}

func configByResolvedClusterKey(clusterName string) client.MatchingFields {
	return client.MatchingFields{IndexConfigByResolvedCluster: clusterName}
}

func roleByServiceAccountKey(saNamespace, saName string) client.MatchingFields {
	return client.MatchingFields{IndexRoleByServiceAccount: serviceAccountIndexKey(saNamespace, saName)}
}

func roleByReplicaSetUIDKey(uid string) client.MatchingFields {
	return client.MatchingFields{IndexRoleByReplicaSetUID: uid}
}

func replicaSetByPlacementRefKey(name string) client.MatchingFields {
	return client.MatchingFields{IndexReplicaSetByPlacementRef: name}
}

// serviceAccountIndexKey is the canonical encoding shared by the index producer
// and consumers; use a single source of truth so the two cannot drift.
func serviceAccountIndexKey(saNamespace, saName string) string {
	return client.ObjectKey{Namespace: saNamespace, Name: saName}.String()
}
