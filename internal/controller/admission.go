// Package controller contains reconcilers, admission validators, and shared controller helpers.
package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
)

// ConfigValidator blocks AWSWorkloadIdentityConfig deletion while role bindings remain.
//
// Create and Update are handled by CRD CEL XValidation rules; this validator
// only owns Delete because it requires a List against namespace-local roles.
type ConfigValidator struct {
	Client client.Reader
}

// ValidateCreate is a no-op.
func (ConfigValidator) ValidateCreate(_ context.Context, _ *identityv1.AWSWorkloadIdentityConfig) (admission.Warnings, error) {
	return nil, nil
}

// ValidateUpdate is a no-op.
func (ConfigValidator) ValidateUpdate(_ context.Context, _, _ *identityv1.AWSWorkloadIdentityConfig) (admission.Warnings, error) {
	return nil, nil
}

// ValidateDelete blocks deletion while namespace role bindings remain.
func (v ConfigValidator) ValidateDelete(ctx context.Context, obj *identityv1.AWSWorkloadIdentityConfig) (admission.Warnings, error) {
	if v.Client == nil {
		return nil, nil
	}

	if obj.IsForceDelete() {
		return nil, nil
	}

	roles := &identityv1.AWSServiceAccountRoleList{}
	if err := v.Client.List(ctx, roles, client.InNamespace(obj.Namespace), client.Limit(1)); err != nil {
		return nil, fmt.Errorf("list AWSServiceAccountRoles in namespace %q: %w", obj.Namespace, err)
	}

	if len(roles.Items) > 0 {
		return nil, fmt.Errorf("cannot delete config while AWSServiceAccountRole objects remain in namespace %q", obj.Namespace)
	}

	return nil, nil
}

// RoleValidator validates AWSServiceAccountRole admission requests.
//
// CRD CEL XValidation already enforces shape and immutability; this validator
// only adds cross-resource checks (sibling Config exists; ServiceAccount is not
// already bound).
type RoleValidator struct {
	Client client.Reader
}

// ValidateCreate runs cross-resource checks.
func (v RoleValidator) ValidateCreate(ctx context.Context, obj *identityv1.AWSServiceAccountRole) (admission.Warnings, error) {
	if v.Client == nil {
		return nil, nil
	}

	config, err := ensureConfigExists(ctx, v.Client, obj.Namespace)
	if err != nil {
		return nil, err
	}

	// Reject creates whose parent Config is being deleted; mirrors the
	// namespace-lifecycle admission semantics where new objects cannot enter
	// a terminating parent.
	if !config.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("AWSWorkloadIdentityConfig/default in namespace %q is being deleted; cannot create new AWSServiceAccountRole", obj.Namespace)
	}

	return nil, ensureNoDuplicateBinding(ctx, v.Client, obj)
}

// ValidateUpdate enforces the sibling-Config invariant only. Duplicate-binding
// uniqueness keys on spec.serviceAccount (CRD-CEL immutable), so it cannot
// change post-Create; the reconciler catches the two-Creates race and surfaces
// it as a status condition. Re-enforcing duplicates here would, under
// failurePolicy: Fail, reject the controller's own finalizer/metadata Updates
// and strand the reconciler before it could write that status. Terminating
// updates skip the Config check so a Role can finish finalization even when
// its Config was force-deleted first (see docs/reference/deletion-behavior.md).
func (v RoleValidator) ValidateUpdate(ctx context.Context, _, newObj *identityv1.AWSServiceAccountRole) (admission.Warnings, error) {
	if !newObj.DeletionTimestamp.IsZero() || v.Client == nil {
		return nil, nil
	}

	_, err := ensureConfigExists(ctx, v.Client, newObj.Namespace)

	return nil, err
}

// ValidateDelete is a no-op.
func (RoleValidator) ValidateDelete(_ context.Context, _ *identityv1.AWSServiceAccountRole) (admission.Warnings, error) {
	return nil, nil
}

func ensureConfigExists(ctx context.Context, c client.Reader, namespace string) (*identityv1.AWSWorkloadIdentityConfig, error) {
	config := &identityv1.AWSWorkloadIdentityConfig{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: identityv1.DefaultName}, config); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("AWSWorkloadIdentityConfig/default is required in namespace %q", namespace)
		}

		return nil, fmt.Errorf("get AWSWorkloadIdentityConfig/default in namespace %q: %w", namespace, err)
	}

	return config, nil
}

func ensureNoDuplicateBinding(ctx context.Context, c client.Reader, obj *identityv1.AWSServiceAccountRole) error {
	roles := &identityv1.AWSServiceAccountRoleList{}
	if err := c.List(ctx, roles, client.InNamespace(obj.Namespace)); err != nil {
		return fmt.Errorf("list AWSServiceAccountRoles in namespace %q: %w", obj.Namespace, err)
	}

	for i := range roles.Items {
		existing := &roles.Items[i]
		// Mirror the active-role semantics in
		// AWSServiceAccountRoleReconciler.conflictingServiceAccountBindingNames:
		// a role with a DeletionTimestamp is being finalized and no longer
		// occupies its ServiceAccount subject.
		if !existing.DeletionTimestamp.IsZero() {
			continue
		}

		if !sameServiceAccountSubject(existing.Spec.ServiceAccount, obj.Spec.ServiceAccount) {
			continue
		}

		return fmt.Errorf("service account %s/%s is already bound by AWSServiceAccountRole %s/%s",
			obj.Spec.ServiceAccount.Namespace, obj.Spec.ServiceAccount.Name,
			existing.Namespace, existing.Name)
	}

	return nil
}

func sameServiceAccountSubject(a, b identityv1.ServiceAccountSubject) bool {
	return a.Namespace == b.Namespace && a.Name == b.Name
}

// RoleReplicaSetValidator validates AWSServiceAccountRoleReplicaSet requests.
type RoleReplicaSetValidator struct{}

// ValidateCreate blocks reserved child metadata in the template.
func (RoleReplicaSetValidator) ValidateCreate(_ context.Context, obj *identityv1.AWSServiceAccountRoleReplicaSet) (admission.Warnings, error) {
	return nil, validateReplicaSetTemplateMetadata(obj)
}

// ValidateUpdate blocks reserved child metadata in the template.
func (RoleReplicaSetValidator) ValidateUpdate(_ context.Context, _, newObj *identityv1.AWSServiceAccountRoleReplicaSet) (admission.Warnings, error) {
	return nil, validateReplicaSetTemplateMetadata(newObj)
}

// ValidateDelete is a no-op.
func (RoleReplicaSetValidator) ValidateDelete(_ context.Context, _ *identityv1.AWSServiceAccountRoleReplicaSet) (admission.Warnings, error) {
	return nil, nil
}

var (
	_ admission.Validator[*identityv1.AWSWorkloadIdentityConfig]       = ConfigValidator{}
	_ admission.Validator[*identityv1.AWSServiceAccountRole]           = RoleValidator{}
	_ admission.Validator[*identityv1.AWSServiceAccountRoleReplicaSet] = RoleReplicaSetValidator{}
)
