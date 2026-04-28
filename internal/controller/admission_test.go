package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
)

var errAdmissionFieldSelectorUsed = errors.New("field selector used")

func TestConfigValidatorBlocksDeleteWhenRolesRemain(t *testing.T) {
	config := testAdmissionConfig(nil)
	role := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: config.Namespace},
		Spec: identityv1.AWSServiceAccountRoleSpec{
			ServiceAccount: identityv1.ServiceAccountSubject{Namespace: "default", Name: "app"},
			PolicyARNs:     []string{"arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess"},
		},
	}
	validator := ConfigValidator{Client: testConfigClient(t, role)}

	if _, err := validator.ValidateDelete(context.Background(), config); err == nil {
		t.Fatal("expected delete to be blocked")
	}
}

func TestConfigValidatorAllowsForceDeleteWhenRolesRemain(t *testing.T) {
	config := testAdmissionConfig(map[string]string{identityv1.ForceDeleteAnnotation: "true"})
	role := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: config.Namespace},
		Spec: identityv1.AWSServiceAccountRoleSpec{
			ServiceAccount: identityv1.ServiceAccountSubject{Namespace: "default", Name: "app"},
			PolicyARNs:     []string{"arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess"},
		},
	}
	validator := ConfigValidator{Client: testConfigClient(t, role)}

	if _, err := validator.ValidateDelete(context.Background(), config); err != nil {
		t.Fatal(err)
	}
}

func TestRoleValidatorBlocksDuplicateServiceAccountBinding(t *testing.T) {
	config := testAdmissionConfig(nil)
	existing := roleForServiceAccountInNamespace("existing", config.Namespace, "app")
	candidate := roleForServiceAccountInNamespace("candidate", config.Namespace, "app")
	validator := RoleValidator{Client: testConfigClient(t, config, existing)}

	if _, err := validator.ValidateCreate(context.Background(), candidate); err == nil {
		t.Fatal("expected duplicate ServiceAccount binding to be blocked")
	}
}

// Mirrors the namespace-lifecycle admission semantics: new children cannot
// enter a parent that is terminating. The candidate uses a previously-unused
// ServiceAccount so any rejection here must come from the terminating-Config
// gate, not the duplicate-binding path.
func TestRoleValidatorRejectsCreateWhenConfigIsTerminating(t *testing.T) {
	config := testAdmissionConfig(nil)
	now := metav1.Now()
	config.DeletionTimestamp = &now
	config.Finalizers = []string{identityv1.ConfigFinalizer}
	candidate := roleForServiceAccountInNamespace("candidate", config.Namespace, "fresh-sa")
	validator := RoleValidator{Client: testConfigClient(t, config)}

	_, err := validator.ValidateCreate(context.Background(), candidate)
	if err == nil {
		t.Fatal("expected create to be rejected while parent Config is terminating")
	}

	if !strings.Contains(err.Error(), "being deleted") {
		t.Fatalf("expected error to mention 'being deleted', got %v", err)
	}

	if !strings.Contains(err.Error(), "AWSWorkloadIdentityConfig") {
		t.Fatalf("expected error to reference AWSWorkloadIdentityConfig, got %v", err)
	}
}

// force-delete is a Config-deletion-gate concept (ConfigValidator.ValidateDelete);
// it must NOT license new child Roles in. Pins design intent so a future change
// can't accidentally let new Roles in by checking the force-delete annotation here.
func TestRoleValidatorRejectsCreateWhenConfigIsTerminatingEvenWithForceDelete(t *testing.T) {
	config := testAdmissionConfig(map[string]string{identityv1.ForceDeleteAnnotation: "true"})
	now := metav1.Now()
	config.DeletionTimestamp = &now
	config.Finalizers = []string{identityv1.ConfigFinalizer}
	candidate := roleForServiceAccountInNamespace("candidate", config.Namespace, "fresh-sa")
	validator := RoleValidator{Client: testConfigClient(t, config)}

	_, err := validator.ValidateCreate(context.Background(), candidate)
	if err == nil {
		t.Fatal("expected create to be rejected while parent Config is terminating (even with force-delete)")
	}

	if !strings.Contains(err.Error(), "being deleted") {
		t.Fatalf("expected error to mention 'being deleted', got %v", err)
	}

	if !strings.Contains(err.Error(), "AWSWorkloadIdentityConfig") {
		t.Fatalf("expected error to reference AWSWorkloadIdentityConfig, got %v", err)
	}
}

// Asymmetry pin: the terminating-Config gate is Create-only. ValidateUpdate
// must keep allowing living-Role updates so the reconciler can keep finalizing
// existing Roles while the parent Config is force-deleting.
func TestRoleValidatorAllowsUpdateWhenConfigIsTerminating(t *testing.T) {
	config := testAdmissionConfig(nil)
	now := metav1.Now()
	config.DeletionTimestamp = &now
	config.Finalizers = []string{identityv1.ConfigFinalizer}
	existing := roleForServiceAccountInNamespace("existing", config.Namespace, "app")
	existing.UID = types.UID(testRoleUID)
	updated := existing.DeepCopy()
	validator := RoleValidator{Client: testConfigClient(t, config, existing)}

	if _, err := validator.ValidateUpdate(context.Background(), existing, updated); err != nil {
		t.Fatalf("expected admission to allow living-Role update while parent Config is terminating, got %v", err)
	}
}

func TestRoleValidatorDoesNotRequireFieldSelectableCRD(t *testing.T) {
	config := testAdmissionConfig(nil)
	existing := roleForServiceAccountInNamespace("existing", config.Namespace, "app")
	candidate := roleForServiceAccountInNamespace("candidate", config.Namespace, "app")
	validator := RoleValidator{Client: noFieldSelectorReader{Reader: testConfigClient(t, config, existing)}}

	_, err := validator.ValidateCreate(context.Background(), candidate)
	if err == nil {
		t.Fatal("expected duplicate ServiceAccount binding to be blocked")
	}

	if errors.Is(err, errAdmissionFieldSelectorUsed) {
		t.Fatal("expected admission validation to list roles without a field selector")
	}

	if !strings.Contains(err.Error(), "already bound") {
		t.Fatalf("expected duplicate binding error, got %v", err)
	}
}

func TestRoleValidatorAllowsDifferentServiceAccountBinding(t *testing.T) {
	config := testAdmissionConfig(nil)
	existing := roleForServiceAccountInNamespace("existing", config.Namespace, "app")
	candidate := roleForServiceAccountInNamespace("candidate", config.Namespace, "other")
	validator := RoleValidator{Client: testConfigClient(t, config, existing)}

	if _, err := validator.ValidateCreate(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
}

// Mirrors AWSServiceAccountRoleReconciler.conflictingServiceAccountBindingNames,
// which treats a role with DeletionTimestamp as no longer occupying the subject.
func TestRoleValidatorAllowsCreateWhenExistingRoleIsTerminating(t *testing.T) {
	config := testAdmissionConfig(nil)
	existing := roleForServiceAccountInNamespace("existing", config.Namespace, "app")
	now := metav1.Now()
	existing.DeletionTimestamp = &now
	existing.Finalizers = []string{identityv1.ServiceAccountRoleFinalizer}
	candidate := roleForServiceAccountInNamespace("candidate", config.Namespace, "app")
	validator := RoleValidator{Client: testConfigClient(t, config, existing)}

	if _, err := validator.ValidateCreate(context.Background(), candidate); err != nil {
		t.Fatalf("expected admission to allow recreation while predecessor is terminating, got %v", err)
	}
}

func TestRoleValidatorAllowsUpdateWhenSiblingRoleIsTerminating(t *testing.T) {
	config := testAdmissionConfig(nil)
	sibling := roleForServiceAccountInNamespace("sibling", config.Namespace, "app")
	now := metav1.Now()
	sibling.DeletionTimestamp = &now
	sibling.Finalizers = []string{identityv1.ServiceAccountRoleFinalizer}
	current := roleForServiceAccountInNamespace("current", config.Namespace, "app")
	current.UID = types.UID(testRoleUID)
	updated := current.DeepCopy()
	validator := RoleValidator{Client: testConfigClient(t, config, sibling, current)}

	if _, err := validator.ValidateUpdate(context.Background(), current, updated); err != nil {
		t.Fatalf("expected admission to allow self-update while a terminating sibling exists, got %v", err)
	}
}

// Duplicate-binding re-check is skipped on Update so the reconciler can write
// its conflict status (see ValidateUpdate doc-comment).
func TestRoleValidatorAllowsLivingRoleUpdateWithDuplicateBindingSibling(t *testing.T) {
	config := testAdmissionConfig(nil)
	sibling := roleForServiceAccountInNamespace("sibling", config.Namespace, "app")
	sibling.UID = types.UID("sibling-uid")
	current := roleForServiceAccountInNamespace("current", config.Namespace, "app")
	current.UID = types.UID(testRoleUID)

	updated := current.DeepCopy()
	updated.Finalizers = append(updated.Finalizers, identityv1.ServiceAccountRoleFinalizer)

	validator := RoleValidator{Client: testConfigClient(t, config, sibling, current)}

	if _, err := validator.ValidateUpdate(context.Background(), current, updated); err != nil {
		t.Fatalf("expected living-Role finalizer update to be allowed despite duplicate-binding sibling, got %v", err)
	}
}

func TestRoleValidatorAllowsSelfUpdate(t *testing.T) {
	config := testAdmissionConfig(nil)
	existing := roleForServiceAccountInNamespace("existing", config.Namespace, "app")
	existing.UID = types.UID(testRoleUID)
	updated := existing.DeepCopy()
	validator := RoleValidator{Client: testConfigClient(t, config, existing)}

	if _, err := validator.ValidateUpdate(context.Background(), existing, updated); err != nil {
		t.Fatal(err)
	}
}

// Force-delete contract: a terminating Role must finish finalization even when
// its Config was force-deleted first (docs/reference/deletion-behavior.md).
func TestRoleValidatorAllowsTerminatingRoleUpdateWhenConfigAbsent(t *testing.T) {
	terminating := roleForServiceAccountInNamespace("orphan", testInventoryNamespace, "app")
	terminating.UID = types.UID(testRoleUID)
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	terminating.Finalizers = []string{identityv1.ServiceAccountRoleFinalizer}

	updated := terminating.DeepCopy()
	updated.Finalizers = nil

	validator := RoleValidator{Client: testConfigClient(t, terminating)}

	if _, err := validator.ValidateUpdate(context.Background(), terminating, updated); err != nil {
		t.Fatalf("expected terminating-Role finalizer-removal update to be allowed when Config is absent, got %v", err)
	}
}

// Terminating Roles must also bypass the duplicate-binding check, since a Role
// being finalized no longer occupies its ServiceAccount subject.
func TestRoleValidatorAllowsTerminatingRoleUpdateEvenWithDuplicateBindingSibling(t *testing.T) {
	config := testAdmissionConfig(nil)
	sibling := roleForServiceAccountInNamespace("sibling", config.Namespace, "app")
	sibling.UID = types.UID("sibling-uid")
	terminating := roleForServiceAccountInNamespace("terminating", config.Namespace, "app")
	terminating.UID = types.UID(testRoleUID)
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	terminating.Finalizers = []string{identityv1.ServiceAccountRoleFinalizer}

	updated := terminating.DeepCopy()
	updated.Finalizers = nil

	validator := RoleValidator{Client: testConfigClient(t, config, sibling, terminating)}

	if _, err := validator.ValidateUpdate(context.Background(), terminating, updated); err != nil {
		t.Fatalf("expected terminating-Role update to bypass duplicate-binding check, got %v", err)
	}
}

// Negative companion: the deletion short-circuit must not leak to living-Role
// Updates — the sibling-Config invariant still applies there.
func TestRoleValidatorStillRequiresConfigForLivingRoleUpdate(t *testing.T) {
	existing := roleForServiceAccountInNamespace("existing", testInventoryNamespace, "app")
	existing.UID = types.UID(testRoleUID)
	updated := existing.DeepCopy()

	validator := RoleValidator{Client: testConfigClient(t, existing)}

	_, err := validator.ValidateUpdate(context.Background(), existing, updated)
	if err == nil {
		t.Fatal("expected update on living role to be rejected when Config is absent")
	}

	if !strings.Contains(err.Error(), "AWSWorkloadIdentityConfig") {
		t.Fatalf("expected error to reference missing AWSWorkloadIdentityConfig, got %v", err)
	}
}

func testAdmissionConfig(annotations map[string]string) *identityv1.AWSWorkloadIdentityConfig {
	return &identityv1.AWSWorkloadIdentityConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:        identityv1.DefaultName,
			Namespace:   testInventoryNamespace,
			Annotations: annotations,
		},
		Spec: identityv1.AWSWorkloadIdentityConfigSpec{
			Type:   identityv1.DeliveryTypeSelfHostedIRSA,
			Region: "ap-northeast-1",
		},
	}
}

type noFieldSelectorReader struct {
	client.Reader
}

func (r noFieldSelectorReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	listOptions := (&client.ListOptions{}).ApplyOptions(opts)
	if listOptions.FieldSelector != nil && !listOptions.FieldSelector.Empty() {
		return errAdmissionFieldSelectorUsed
	}

	if listOptions.Raw != nil && listOptions.Raw.FieldSelector != "" {
		return errAdmissionFieldSelectorUsed
	}

	if err := r.Reader.List(ctx, list, opts...); err != nil {
		return fmt.Errorf("noFieldSelectorReader List: %w", err)
	}

	return nil
}
