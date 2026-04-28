package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ACKResourceStatus reports the status copied from an operator-owned ACK child.
type ACKResourceStatus struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	// +listType=map
	// +listMapKey=type
	Conditions []ACKResourceCondition `json:"conditions,omitempty"`
}

// ACKResourceCondition mirrors the public shape of ACK status conditions.
type ACKResourceCondition struct {
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=316
	Type string `json:"type"`
	// +kubebuilder:validation:Enum=True;False;Unknown
	Status             corev1.ConditionStatus `json:"status"`
	LastTransitionTime *metav1.Time           `json:"lastTransitionTime,omitempty"`
	// +kubebuilder:validation:MaxLength=1024
	Reason string `json:"reason,omitempty"`
	// +kubebuilder:validation:MaxLength=32768
	Message string `json:"message,omitempty"`
}
