package aws

import (
	"testing"

	ackv1alpha1 "github.com/aws-controllers-k8s/runtime/apis/core/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
)

func TestACKResourceStatusCopiesConditions(t *testing.T) {
	reason := "Synced"
	message := "resource is synced"
	now := metav1.Now()
	obj := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "child", Namespace: "wlc-a"}}

	status := ACKResourceStatus("example.io/v1", "Example", obj, []*ackv1alpha1.Condition{
		nil,
		{
			Type:               ackv1alpha1.ConditionTypeResourceSynced,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: &now,
			Reason:             &reason,
			Message:            &message,
		},
	})

	if status.APIVersion != "example.io/v1" || status.Kind != "Example" || status.Namespace != "wlc-a" || status.Name != "child" {
		t.Fatalf("unexpected resource identity: %#v", status)
	}

	if len(status.Conditions) != 1 {
		t.Fatalf("expected one copied condition, got %d", len(status.Conditions))
	}

	condition := status.Conditions[0]
	if condition.Type != string(ackv1alpha1.ConditionTypeResourceSynced) || condition.Status != corev1.ConditionTrue {
		t.Fatalf("unexpected copied condition: %#v", condition)
	}

	if condition.Reason != reason || condition.Message != message || condition.LastTransitionTime == nil {
		t.Fatalf("expected optional fields copied: %#v", condition)
	}
}

func TestBuildIAMRoleSetsACKLateInitializedDefaults(t *testing.T) {
	role := &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "wlc-a"},
	}

	built := BuildIAMRole(role, identityv1.DeliveryTypeSelfHostedIRSA, "", "{}", nil)

	if built.Spec.Path == nil || *built.Spec.Path != "/" {
		t.Fatalf("expected path default to be explicit, got %#v", built.Spec.Path)
	}

	if built.Spec.MaxSessionDuration == nil || *built.Spec.MaxSessionDuration != 3600 {
		t.Fatalf("expected max session duration default to be explicit, got %#v", built.Spec.MaxSessionDuration)
	}
}
