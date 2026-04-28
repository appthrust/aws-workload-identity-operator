package remoteirsa

import (
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/types"
)

// ErrorReason is a stable machine-readable reason for a remote IRSA failure.
type ErrorReason string

const (
	// ReasonInvalidOptions means the provider options are incomplete or invalid.
	ReasonInvalidOptions ErrorReason = "InvalidOptions"
	// ReasonConfigNotFound means AWSWorkloadIdentityConfig/default was not found.
	ReasonConfigNotFound ErrorReason = "ConfigNotFound"
	// ReasonUnsupportedDeliveryType means the resolved delivery type cannot serve remote IRSA.
	ReasonUnsupportedDeliveryType ErrorReason = "UnsupportedDeliveryType"
	// ReasonRoleNotFound means no usable AWSServiceAccountRole was resolved.
	ReasonRoleNotFound ErrorReason = "RoleNotFound"
	// ReasonMultipleRoles means more than one AWSServiceAccountRole matched the ServiceAccount.
	ReasonMultipleRoles ErrorReason = "MultipleRoles"
	// ReasonMultipleClusterProfiles means more than one OCM ClusterProfile carrying
	// AccessProviders matched the workload namespace cluster-name label. The
	// resolver fails closed rather than route remote credentials at an arbitrary
	// collision winner.
	ReasonMultipleClusterProfiles ErrorReason = "MultipleClusterProfiles"
	// ReasonRoleServiceAccountMismatch means the explicit role targets a different ServiceAccount.
	ReasonRoleServiceAccountMismatch ErrorReason = "RoleServiceAccountMismatch"
	// ReasonRoleARNNotReady means the role status has not published a role ARN yet.
	ReasonRoleARNNotReady ErrorReason = "RoleARNNotReady"
	// ReasonConfigNotReady means the AWSWorkloadIdentityConfig status has not caught up with spec.
	ReasonConfigNotReady ErrorReason = "ConfigNotReady"
	// ReasonRegionNotReady means no STS region could be resolved.
	ReasonRegionNotReady ErrorReason = "RegionNotReady"
	// ReasonMissingInventoryAccess means Cluster Inventory access data was unavailable.
	ReasonMissingInventoryAccess ErrorReason = "MissingInventoryAccess"
	// ReasonRemoteTokenRequestForbidden means the remote API denied TokenRequest.
	ReasonRemoteTokenRequestForbidden ErrorReason = "RemoteTokenRequestForbidden"
	// ReasonRemoteTokenRequestFailed means the remote TokenRequest failed.
	ReasonRemoteTokenRequestFailed ErrorReason = "RemoteTokenRequestFailed"
	// ReasonSTSAccessDenied means STS denied AssumeRoleWithWebIdentity.
	ReasonSTSAccessDenied ErrorReason = "STSAccessDenied"
	// ReasonExpiredOrInvalidToken means STS rejected the web identity token.
	ReasonExpiredOrInvalidToken ErrorReason = "ExpiredOrInvalidToken"
	// ReasonSTSCredentialsUnavailable means STS returned no usable credentials.
	ReasonSTSCredentialsUnavailable ErrorReason = "STSCredentialsUnavailable"
	// ReasonSTSAssumeRoleWithWebIdentity means STS AssumeRoleWithWebIdentity failed.
	ReasonSTSAssumeRoleWithWebIdentity ErrorReason = "STSAssumeRoleWithWebIdentityFailed"
)

// Error is a typed, redacted SDK error. It intentionally records object
// references and reason data, but never stores tokens or AWS secret material.
type Error struct {
	reason ErrorReason
	msg    string
	err    error

	workloadNamespace string
	serviceAccount    types.NamespacedName
	roleRef           types.NamespacedName
	clusterProfileRef types.NamespacedName
	providerName      string
}

// Reason returns the stable reason for the error.
func (e *Error) Reason() ErrorReason {
	if e == nil {
		return ""
	}

	return e.reason
}

// WorkloadNamespace returns the hub namespace used for AWS identity objects.
func (e *Error) WorkloadNamespace() string {
	if e == nil {
		return ""
	}

	return e.workloadNamespace
}

// ServiceAccount returns the remote ServiceAccount involved in the failure.
func (e *Error) ServiceAccount() types.NamespacedName {
	if e == nil {
		return types.NamespacedName{}
	}

	return e.serviceAccount
}

// RoleRef returns the hub-side AWSServiceAccountRole involved in the failure.
func (e *Error) RoleRef() types.NamespacedName {
	if e == nil {
		return types.NamespacedName{}
	}

	return e.roleRef
}

// ClusterProfileRef returns the ClusterProfile involved in remote access
// resolution, when one was resolved before the failure.
func (e *Error) ClusterProfileRef() types.NamespacedName {
	if e == nil {
		return types.NamespacedName{}
	}

	return e.clusterProfileRef
}

// ProviderName returns the Cluster Inventory access provider selected for the
// remote Kubernetes rest.Config, when one was resolved before the failure.
func (e *Error) ProviderName() string {
	if e == nil {
		return ""
	}

	return e.providerName
}

// Error implements error.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}

	out := fmt.Sprintf("remoteirsa %s: %s", e.reason, e.msg)

	if fields := e.contextFields(); len(fields) > 0 {
		out += " (" + strings.Join(fields, " ") + ")"
	}

	if e.err != nil {
		out += ": " + e.err.Error()
	}

	return out
}

func (e *Error) contextFields() []string {
	fields := make([]string, 0, 5)

	if e.workloadNamespace != "" {
		fields = append(fields, "workloadNamespace="+e.workloadNamespace)
	}

	if e.serviceAccount != (types.NamespacedName{}) {
		fields = append(fields, "serviceAccount="+e.serviceAccount.String())
	}

	if e.roleRef != (types.NamespacedName{}) {
		fields = append(fields, "role="+e.roleRef.String())
	}

	if e.clusterProfileRef != (types.NamespacedName{}) {
		fields = append(fields, "clusterProfile="+e.clusterProfileRef.String())
	}

	if e.providerName != "" {
		fields = append(fields, "provider="+e.providerName)
	}

	return fields
}

// Unwrap returns the underlying cause.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}

	return e.err
}

// Reason returns a typed reason if err contains a remoteirsa Error.
func Reason(err error) ErrorReason {
	var sdkErr *Error
	if errors.As(err, &sdkErr) {
		return sdkErr.reason
	}

	return ""
}

// Temporary reports whether a later Retrieve may succeed after external state
// changes or after requesting a fresh web identity token. It is informational
// only; callers own their retry policy.
func Temporary(err error) bool {
	switch Reason(err) {
	case ReasonInvalidOptions,
		ReasonConfigNotFound,
		ReasonUnsupportedDeliveryType,
		ReasonRoleNotFound,
		ReasonMultipleRoles,
		ReasonMultipleClusterProfiles,
		ReasonRoleServiceAccountMismatch,
		"":
		return false
	case ReasonRoleARNNotReady,
		ReasonConfigNotReady,
		ReasonRegionNotReady,
		ReasonMissingInventoryAccess,
		ReasonRemoteTokenRequestForbidden,
		ReasonRemoteTokenRequestFailed,
		ReasonSTSAccessDenied,
		ReasonExpiredOrInvalidToken,
		ReasonSTSCredentialsUnavailable,
		ReasonSTSAssumeRoleWithWebIdentity:
		return true
	default:
		return false
	}
}

type errorContext struct {
	workloadNamespace string
	serviceAccount    types.NamespacedName
	roleRef           types.NamespacedName
	clusterProfileRef types.NamespacedName
	providerName      string
}

func newError(reason ErrorReason, msg string, err error, ctx errorContext) *Error { //nolint:gocritic // Keep call sites readable; this context is copied into immutable errors.
	return &Error{
		reason:            reason,
		msg:               msg,
		err:               err,
		workloadNamespace: ctx.workloadNamespace,
		serviceAccount:    ctx.serviceAccount,
		roleRef:           ctx.roleRef,
		clusterProfileRef: ctx.clusterProfileRef,
		providerName:      ctx.providerName,
	}
}

func withErrorContext(err error, ctx errorContext) error { //nolint:gocritic // Keep call sites readable; this context is copied into a wrapped error.
	if err == nil {
		return nil
	}

	var sdkErr *Error
	if !errors.As(err, &sdkErr) {
		return err
	}

	out := *sdkErr
	if out.workloadNamespace == "" {
		out.workloadNamespace = ctx.workloadNamespace
	}

	if out.serviceAccount == (types.NamespacedName{}) {
		out.serviceAccount = ctx.serviceAccount
	}

	if out.roleRef == (types.NamespacedName{}) {
		out.roleRef = ctx.roleRef
	}

	if out.clusterProfileRef == (types.NamespacedName{}) {
		out.clusterProfileRef = ctx.clusterProfileRef
	}

	if out.providerName == "" {
		out.providerName = ctx.providerName
	}

	return &out
}
