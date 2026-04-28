package controller

import (
	corev1 "k8s.io/api/core/v1"
	toolscache "k8s.io/client-go/tools/cache"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
)

// LocalNamespaceCacheByObject returns cache options for the local manager's
// cluster-wide Namespace informer. The AWSServiceAccountRoleReplicaSet
// reconciler watches Namespace creations to retry per-cluster apply when a
// previously missing namespace appears, so the cache holds every Namespace
// in the cluster. TransformStripManagedFields drops metav1.ManagedFields
// from cached Namespace objects: nothing in this operator reads those
// fields, and Namespaces in multi-tenant clusters accumulate large
// per-actor managedFields entries. Stripping them bounds per-Namespace
// memory in the informer cache without affecting reconcile semantics.
func LocalNamespaceCacheByObject() map[client.Object]crcache.ByObject {
	return map[client.Object]crcache.ByObject{
		&corev1.Namespace{}: {
			Transform: crcache.TransformStripManagedFields(),
		},
	}
}

// LocalSecretCacheByObject returns cache options for the local manager's
// Secret watch. Kubernetes RBAC cannot filter Secrets by label and
// AWSWorkloadIdentityConfig signing-key Secrets live in arbitrary
// namespaces, so the operator must keep cluster-wide secrets verbs and the
// informer must list every Secret in the cluster to back
// Owns(&corev1.Secret{}) plus the foreign-Secret detection in
// deleteConfigChildIfOwned. The Transform strips Data/StringData on cached
// Secrets that this operator does not manage so that an in-process
// compromise of the controller cannot read foreign Secret material. Managed
// signing-key Secrets keep Data intact because reconcileSigningSecret reads
// it back through the cached client and would silently regenerate the key
// if it ever observed an empty Data on a managed Secret.
func LocalSecretCacheByObject() map[client.Object]crcache.ByObject {
	return map[client.Object]crcache.ByObject{
		&corev1.Secret{}: {
			Namespaces: map[string]crcache.Config{},
			Transform:  stripForeignSecretDataForCache(),
		},
	}
}

func stripForeignSecretDataForCache() toolscache.TransformFunc {
	stripManagedFields := crcache.TransformStripManagedFields()

	return func(in any) (any, error) {
		out, err := stripManagedFields(in)
		if err != nil {
			return nil, err
		}

		secret, ok := out.(*corev1.Secret)
		if !ok {
			return out, nil
		}

		// Either label is sufficient to recognise a managed Secret so that hand
		// editing one of them (e.g. kubectl label --overwrite) does not cause
		// the cache to silently drop key material from a Secret that
		// reconcileSigningSecret still depends on.
		if secret.Labels[identityv1.LabelManagedBy] == identityv1.ManagedByValue {
			return out, nil
		}

		if secret.Labels[identityv1.LabelConfigUID] != "" {
			return out, nil
		}

		secret.Data = nil
		secret.StringData = nil

		return out, nil
	}
}
