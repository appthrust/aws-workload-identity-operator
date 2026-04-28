// Package v1alpha1 contains API Schema definitions for the aws.identity.appthrust.io API group.
// +kubebuilder:object:generate=true
// +groupName=aws.identity.appthrust.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

const (
	// Group is the Kubernetes API group.
	Group = "aws.identity.appthrust.io"
	// Version is the Kubernetes API version.
	Version = "v1alpha1"
)

var (
	// GroupVersion identifies this API group and version.
	GroupVersion = schema.GroupVersion{Group: Group, Version: Version}
	// SchemeBuilder registers this API group with a runtime scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	// AddToScheme adds this API group to a runtime scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(
		&AWSWorkloadIdentityConfig{},
		&AWSWorkloadIdentityConfigList{},
		&AWSServiceAccountRole{},
		&AWSServiceAccountRoleList{},
		&AWSWorkloadIdentityOperatorConfig{},
		&AWSWorkloadIdentityOperatorConfigList{},
	)
}
