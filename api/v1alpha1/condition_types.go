package v1alpha1

// API constants for object names, labels, annotations, and finalizers.
const (
	DefaultName = "default"

	ConfigFinalizer                       = "aws.identity.appthrust.io/config-finalizer"
	ServiceAccountRoleFinalizer           = "aws.identity.appthrust.io/service-account-role-finalizer"
	ServiceAccountRoleReplicaSetFinalizer = "aws.identity.appthrust.io/service-account-role-replicaset-finalizer"

	LabelManagedBy      = "app.kubernetes.io/managed-by"
	LabelConfigUID      = "aws.identity.appthrust.io/config-uid"
	LabelBindingUID     = "aws.identity.appthrust.io/binding-uid"
	LabelInventoryNS    = "aws.identity.appthrust.io/inventory-namespace"
	LabelOwnerRef       = "aws.identity.appthrust.io/owner-ref"
	LabelServiceAccount = "aws.identity.appthrust.io/service-account"
	LabelDelivery       = "aws.identity.appthrust.io/delivery"
	LabelRuntime        = "aws.identity.appthrust.io/runtime"
	LabelReplicaSetUID  = "aws.identity.appthrust.io/replicaset-uid"

	AnnotationSigningKeyID       = "aws.identity.appthrust.io/signing-key-id"
	AnnotationReplicaSetOwnerRef = "aws.identity.appthrust.io/replicaset-owner-ref"

	ManagedByValue = "aws-workload-identity-operator"
	RuntimeWebhook = "self-hosted-webhook"

	ForceDeleteAnnotation = "aws.identity.appthrust.io/force-delete"
)

// DeliveryType selects the workload identity delivery strategy.
type DeliveryType string

// Delivery type constants supported by AWSWorkloadIdentityConfig.
const (
	DeliveryTypeSelfHostedIRSA DeliveryType = "SelfHostedIRSA"
	DeliveryTypeEKSPodIdentity DeliveryType = "EKSPodIdentity"
)

// Condition types reported by the controllers.
const (
	ConditionOperatorConfigReady           = "OperatorConfigReady"
	ConditionClusterProfileResolved        = "ClusterProfileResolved"
	ConditionInventoryResolved             = "InventoryResolved"
	ConditionBucketReady                   = "BucketReady"
	ConditionOIDCObjectsPublished          = "OIDCObjectsPublished"
	ConditionIAMProviderReady              = "IAMProviderReady"
	ConditionIssuerReady                   = "IssuerReady"
	ConditionConfigResolved                = "ConfigResolved"
	ConditionPolicyReady                   = "PolicyReady"
	ConditionRoleReady                     = "RoleReady"
	ConditionTrustPolicyReady              = "TrustPolicyReady"
	ConditionServiceAccountAnnotationReady = "ServiceAccountAnnotationReady"
	ConditionWebhookRuntimeReady           = "WebhookRuntimeReady"
	ConditionPodIdentityAssocReady         = "PodIdentityAssociationReady"
	ConditionPodIdentityAgentReady         = "PodIdentityAgentReady"
	ConditionDeliveryReady                 = "DeliveryReady"
	ConditionReady                         = "Ready"
	ConditionDeletionBlocked               = "DeletionBlocked"
	ConditionPlacementResolved             = "PlacementResolved"
	ConditionAWSServiceAccountRolesApplied = "AWSServiceAccountRolesApplied"
	ConditionAWSServiceAccountRolesReady   = "AWSServiceAccountRolesReady"
	ConditionCleanupBlocked                = "CleanupBlocked"
)

// Reason* values are stable condition reasons emitted by the controllers and
// event reasons surfaced via the EventRecorder. AllReasons() is the single
// source of truth used by the metrics allowlist so a typo cannot silently
// coerce a metric label to "other".
const (
	ReasonReady                       = "Ready"
	ReasonAnnotationRepaired          = "AnnotationRepaired"
	ReasonReconciled                  = "Reconciled"
	ReasonResolved                    = "Resolved"
	ReasonRendered                    = "Rendered"
	ReasonNotRequired                 = "NotRequired"
	ReasonHubResourcesReady           = "HubResourcesReady"
	ReasonEKSAutoMode                 = "EKSAutoMode"
	ReasonManagedPoliciesOnly         = "ManagedPoliciesOnly"
	ReasonInvalidSpec                 = "InvalidSpec"
	ReasonOperatorConfigUnavailable   = "OperatorConfigUnavailable"
	ReasonConfigNotReady              = "ConfigNotReady"
	ReasonConfigUnavailable           = "ConfigUnavailable"
	ReasonResolverError               = "ResolverError"
	ReasonClusterProfileNotFound      = "ClusterProfileNotFound"
	ReasonTrustPolicyInputMissing     = "TrustPolicyInputMissing"
	ReasonRolesRemain                 = "RolesRemain"
	ReasonACKResourceSynced           = "ACKResourceSynced"
	ReasonACKResourceNotSynced        = "ACKResourceNotSynced"
	ReasonWaitingForACK               = "WaitingForACK"
	ReasonRemoteCheckPending          = "RemoteCheckPending"
	ReasonRemoteDeliveryPending       = "RemoteDeliveryPending"
	ReasonDeletionBlocked             = "DeletionBlocked"
	ReasonInventoryUnavailable        = "InventoryUnavailable"
	ReasonACKResourceWaiting          = "ACKResourceWaiting"
	ReasonIssuerReconcileFailed       = "IssuerReconcileFailed"
	ReasonOIDCObjectsPublished        = "OIDCObjectsPublished"
	ReasonOIDCObjectsPublishFailed    = "OIDCObjectsPublishFailed"
	ReasonRemoteClusterUnavailable    = "RemoteClusterUnavailable"
	ReasonWebhookRuntimeApplyFailed   = "WebhookRuntimeApplyFailed"
	ReasonWebhookRuntimeSynced        = "WebhookRuntimeSynced"
	ReasonWebhookRuntimeUnavailable   = "WebhookRuntimeUnavailable"
	ReasonWaitingForWebhookDeployment = "WaitingForWebhookDeployment"
	ReasonPlacementUnavailable        = "PlacementUnavailable"
	ReasonPlacementUnsupported        = "PlacementUnsupported"
	ReasonChildApplyFailed            = "ChildApplyFailed"
	ReasonChildConflict               = "ChildConflict"
	ReasonChildrenApplied             = "ChildrenApplied"
	ReasonChildrenPending             = "ChildrenPending"
	ReasonChildrenReady               = "ChildrenReady"
	ReasonClusterNamespaceMissing     = "ClusterNamespaceMissing"
	ReasonImmutableChildDrift         = "ImmutableChildDrift"
	ReasonDeletionUnblocked           = "DeletionUnblocked"
)

// AllReasons returns every Reason* value above. Used by the metrics package to
// build a label allowlist without re-listing constants and risking drift.
func AllReasons() []string {
	return []string{
		ReasonReady,
		ReasonAnnotationRepaired,
		ReasonReconciled,
		ReasonResolved,
		ReasonRendered,
		ReasonNotRequired,
		ReasonHubResourcesReady,
		ReasonEKSAutoMode,
		ReasonManagedPoliciesOnly,
		ReasonInvalidSpec,
		ReasonOperatorConfigUnavailable,
		ReasonConfigNotReady,
		ReasonConfigUnavailable,
		ReasonResolverError,
		ReasonClusterProfileNotFound,
		ReasonTrustPolicyInputMissing,
		ReasonRolesRemain,
		ReasonACKResourceSynced,
		ReasonACKResourceNotSynced,
		ReasonWaitingForACK,
		ReasonRemoteCheckPending,
		ReasonRemoteDeliveryPending,
		ReasonDeletionBlocked,
		ReasonInventoryUnavailable,
		ReasonACKResourceWaiting,
		ReasonIssuerReconcileFailed,
		ReasonOIDCObjectsPublished,
		ReasonOIDCObjectsPublishFailed,
		ReasonRemoteClusterUnavailable,
		ReasonWebhookRuntimeApplyFailed,
		ReasonWebhookRuntimeSynced,
		ReasonWebhookRuntimeUnavailable,
		ReasonWaitingForWebhookDeployment,
		ReasonPlacementUnavailable,
		ReasonPlacementUnsupported,
		ReasonChildApplyFailed,
		ReasonChildConflict,
		ReasonChildrenApplied,
		ReasonChildrenPending,
		ReasonChildrenReady,
		ReasonClusterNamespaceMissing,
		ReasonImmutableChildDrift,
		ReasonDeletionUnblocked,
	}
}
