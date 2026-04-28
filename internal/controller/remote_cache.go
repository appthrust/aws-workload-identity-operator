package controller

import (
	corev1 "k8s.io/api/core/v1"
	toolscache "k8s.io/client-go/tools/cache"
	crcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RemoteServiceAccountCacheByObject returns cache options for remote
// ServiceAccount watches. Kubernetes cannot label-select annotations, so the
// informer still watches ServiceAccounts broadly; the transform trims fields
// this operator never reads to reduce per-cluster cache memory.
func RemoteServiceAccountCacheByObject() map[client.Object]crcache.ByObject {
	return map[client.Object]crcache.ByObject{
		&corev1.ServiceAccount{}: {
			Namespaces: map[string]crcache.Config{},
			Transform:  trimRemoteServiceAccountForCache(),
		},
	}
}

// RemoteServiceAccountUncachedReadObjects keeps client reads live for
// ServiceAccounts. The informer still watches trimmed objects for events, but
// reconcilers must not feed those trimmed cache objects into full Update paths.
func RemoteServiceAccountUncachedReadObjects() []client.Object {
	return []client.Object{&corev1.ServiceAccount{}}
}

func trimRemoteServiceAccountForCache() toolscache.TransformFunc {
	stripManagedFields := crcache.TransformStripManagedFields()

	return func(in any) (any, error) {
		out, err := stripManagedFields(in)
		if err != nil {
			return nil, err
		}

		if serviceAccount, ok := out.(*corev1.ServiceAccount); ok {
			serviceAccount.Secrets = nil
			serviceAccount.ImagePullSecrets = nil
		}

		return out, nil
	}
}
