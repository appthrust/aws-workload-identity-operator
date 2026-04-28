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
	return nil, v.validate(ctx, nil, obj)
}

// ValidateUpdate runs cross-resource checks.
func (v RoleValidator) ValidateUpdate(ctx context.Context, oldObj, newObj *identityv1.AWSServiceAccountRole) (admission.Warnings, error) {
	return nil, v.validate(ctx, oldObj, newObj)
}

// ValidateDelete is a no-op.
func (RoleValidator) ValidateDelete(_ context.Context, _ *identityv1.AWSServiceAccountRole) (admission.Warnings, error) {
	return nil, nil
}

func (v RoleValidator) validate(ctx context.Context, oldObj, obj *identityv1.AWSServiceAccountRole) error {
	if v.Client == nil {
		return nil
	}

	if err := ensureConfigExists(ctx, v.Client, obj.Namespace); err != nil {
		return err
	}

	return ensureNoDuplicateBinding(ctx, v.Client, oldObj, obj)
}

// OperatorConfigValidator is intentionally a no-op. The webhook is still
// registered so future cross-field validation can be added without changing the
// chart's admission surface.
type OperatorConfigValidator struct{}

// ValidateCreate is a no-op.
func (OperatorConfigValidator) ValidateCreate(_ context.Context, _ *identityv1.AWSWorkloadIdentityOperatorConfig) (admission.Warnings, error) {
	return nil, nil
}

// ValidateUpdate is a no-op.
func (OperatorConfigValidator) ValidateUpdate(_ context.Context, _, _ *identityv1.AWSWorkloadIdentityOperatorConfig) (admission.Warnings, error) {
	return nil, nil
}

// ValidateDelete is a no-op.
func (OperatorConfigValidator) ValidateDelete(_ context.Context, _ *identityv1.AWSWorkloadIdentityOperatorConfig) (admission.Warnings, error) {
	return nil, nil
}

func ensureConfigExists(ctx context.Context, c client.Reader, namespace string) error {
	config := &identityv1.AWSWorkloadIdentityConfig{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: identityv1.DefaultName}, config); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("AWSWorkloadIdentityConfig/default is required in namespace %q", namespace)
		}

		return fmt.Errorf("get AWSWorkloadIdentityConfig/default in namespace %q: %w", namespace, err)
	}

	return nil
}

func ensureNoDuplicateBinding(ctx context.Context, c client.Reader, oldObj, obj *identityv1.AWSServiceAccountRole) error {
	roles := &identityv1.AWSServiceAccountRoleList{}
	if err := c.List(ctx, roles, client.InNamespace(obj.Namespace)); err != nil {
		return fmt.Errorf("list AWSServiceAccountRoles in namespace %q: %w", obj.Namespace, err)
	}

	for i := range roles.Items {
		existing := &roles.Items[i]
		if oldObj != nil && existing.UID == oldObj.UID {
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

var (
	_ admission.Validator[*identityv1.AWSWorkloadIdentityConfig]         = ConfigValidator{}
	_ admission.Validator[*identityv1.AWSServiceAccountRole]             = RoleValidator{}
	_ admission.Validator[*identityv1.AWSWorkloadIdentityOperatorConfig] = OperatorConfigValidator{}
)
