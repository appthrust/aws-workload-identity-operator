// Package metrics records bounded Prometheus metrics for controller activity.
package metrics

import (
	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
)

// Controller* are the controller name labels emitted on awio_* metrics.
// Sharing these constants with the controller package avoids silent metric
// coercion when a controller passes a name not on the allowlist.
const (
	ControllerConfig                   = "AWSWorkloadIdentityConfig"
	ControllerRole                     = "AWSServiceAccountRole"
	ControllerRoleReplicaSet           = "AWSServiceAccountRoleReplicaSet"
	ControllerSelfHostedRoleEnqueue    = "selfhosted-role-enqueue"
	ControllerSelfHostedServiceAccount = "selfhosted-serviceaccount"
	ControllerSelfHostedWebhook        = "selfhosted-webhook-runtime"
)

// RemoteDeliveryResult* are the result label values used on awio_remote_delivery_total.
const (
	RemoteDeliveryResultError   = "error"
	RemoteDeliveryResultSkipped = "skipped"
	RemoteDeliveryResultSuccess = "success"
)

// RemoteDeliveryReason* are bounded reason label values used on
// awio_remote_delivery_total. They are deliberately stable strings rather than
// condition reasons because they describe transport-level skip/error states
// that have no condition equivalent.
const (
	RemoteDeliveryReasonWaitingInventory  = "waiting_inventory"
	RemoteDeliveryReasonNotSelfHosted     = "not_self_hosted"
	RemoteDeliveryReasonClusterUnavail    = "cluster_unavailable"
	RemoteDeliveryReasonNoNamespace       = "no_inventory_namespace"
	RemoteDeliveryReasonApplyFailed       = "apply_failed"
	RemoteDeliveryReasonIndexLookupFailed = "index_lookup_failed"
	RemoteDeliveryReasonEnqueued          = "enqueued"
	RemoteDeliveryReasonChannelFull       = "channel_full"
	RemoteDeliveryReasonStaleClusterEvent = "stale_cluster_event"
)

// PredicateDecision* are bounded decision label values used on
// awio_predicate_decision_total.
const (
	PredicateDecisionKept    = "kept"
	PredicateDecisionDropped = "dropped"
)

// Resource* are bounded resource label values used on awio_remote_delivery_total.
const (
	ResourceServiceAccount = "ServiceAccount"
	ResourceWebhookRuntime = "WebhookRuntime"
)

// AllRemoteDeliveryReasons enumerates every RemoteDeliveryReason* value. Adding
// a new reason requires updating both the const block above and this slice.
var AllRemoteDeliveryReasons = []string{
	RemoteDeliveryReasonWaitingInventory,
	RemoteDeliveryReasonNotSelfHosted,
	RemoteDeliveryReasonClusterUnavail,
	RemoteDeliveryReasonNoNamespace,
	RemoteDeliveryReasonApplyFailed,
	RemoteDeliveryReasonIndexLookupFailed,
	RemoteDeliveryReasonEnqueued,
	RemoteDeliveryReasonChannelFull,
	RemoteDeliveryReasonStaleClusterEvent,
}

var allDeliveryTypes = []string{
	string(identityv1.DeliveryTypeSelfHostedIRSA),
	string(identityv1.DeliveryTypeEKSPodIdentity),
	string(identityv1.DeliveryTypeEKSIRSA),
}
