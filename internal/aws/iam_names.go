package aws

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"regexp"
	"strings"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
)

const (
	// namePrefix is the deterministic prefix applied to every operator-owned
	// AWS resource name. Short and DNS-safe so it survives bucket/role/policy
	// length caps.
	namePrefix = "awi"

	suffixHexChars = 10
	dnsNameLimit   = 63
	iamNameLimit   = 64
	iamPolicyLimit = 128
)

// SigningKeyPrivateKey and SigningKeyPublicKey are the keys under which the
// signing Secret stores the OIDC issuer's private/public PEM. The names form
// a stable contract with the aws-pod-identity-webhook image, which mounts the
// same Secret and reads these keys.
const (
	SigningKeyPrivateKey = "sa.key"
	SigningKeyPublicKey  = "sa.pub"
)

var dnsUnsafe = regexp.MustCompile(`[^a-z0-9-]+`)

func safeDNSPart(s string) string {
	s = strings.ToLower(s)
	s = dnsUnsafe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")

	if s == "" {
		return "x"
	}

	return s
}

func suffix(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "/")))

	return hex.EncodeToString(h[:])[:suffixHexChars]
}

func boundedName(base string, limit int, suffixParts ...string) string {
	tail := suffix(suffixParts...)

	full := base + "-" + tail
	if len(full) <= limit {
		return full
	}

	prefix := strings.TrimRight(full[:limit-len(tail)-1], "-")

	return prefix + "-" + tail
}

// joinedBase concatenates the prefix, an optional kind tag, and DNS-safe parts
// with hyphens.
func joinedBase(kind string, parts ...string) string {
	segments := make([]string, 0, 2+len(parts))
	segments = append(segments, namePrefix)

	if kind != "" {
		segments = append(segments, kind)
	}

	for _, p := range parts {
		segments = append(segments, safeDNSPart(p))
	}

	return strings.Join(segments, "-")
}

// BucketName returns the deterministic S3 bucket name for a namespace region.
func BucketName(namespace, region string) string {
	return boundedName(joinedBase("", namespace, region), dnsNameLimit, namespace, region)
}

// RoleName returns the deterministic IAM Role name for a workload role.
func RoleName(role *identityv1.AWSServiceAccountRole) string {
	return boundedName(joinedBase("", role.Namespace, role.Name), iamNameLimit, string(role.UID), role.Namespace, role.Name)
}

// PolicyName returns the deterministic IAM Policy name for a workload role.
func PolicyName(role *identityv1.AWSServiceAccountRole) string {
	return boundedName(joinedBase("pol", role.Namespace, role.Name), iamPolicyLimit, string(role.UID), "policy", role.Namespace, role.Name)
}

// OIDCProviderName returns the ACK OIDC provider resource name for a config.
func OIDCProviderName(config *identityv1.AWSWorkloadIdentityConfig) string {
	return joinedBase("oidc", config.Namespace)
}

// SigningKeySecretName returns the signing key Secret name for a config.
func SigningKeySecretName(config *identityv1.AWSWorkloadIdentityConfig) string {
	return joinedBase("signing-key", config.Name)
}

// PodIdentityAssociationName returns the ACK PodIdentityAssociation name for a role.
func PodIdentityAssociationName(role *identityv1.AWSServiceAccountRole) string {
	return joinedBase("pia", role.Name)
}

func ownerLabelValue(namespace, name string) string {
	return boundedName(safeDNSPart(namespace)+"-"+safeDNSPart(name), dnsNameLimit, namespace, name)
}

func baseLabels(ownerRef string) map[string]string {
	return map[string]string{
		identityv1.LabelManagedBy: identityv1.ManagedByValue,
		identityv1.LabelOwnerRef:  ownerRef,
	}
}

// LabelsForConfig returns common labels for resources owned by a config.
func LabelsForConfig(config *identityv1.AWSWorkloadIdentityConfig) map[string]string {
	labels := baseLabels(ownerLabelValue(config.Namespace, config.Name))
	maps.Copy(labels, map[string]string{
		identityv1.LabelConfigUID: string(config.UID),
		identityv1.LabelDelivery:  string(config.Spec.Type),
	})

	return labels
}

// LabelsForRole returns common labels for resources owned by a role.
func LabelsForRole(role *identityv1.AWSServiceAccountRole, delivery identityv1.DeliveryType) map[string]string {
	labels := baseLabels(ownerLabelValue(role.Namespace, role.Name))
	maps.Copy(labels, map[string]string{
		identityv1.LabelBindingUID:     string(role.UID),
		identityv1.LabelServiceAccount: ownerLabelValue(role.Spec.ServiceAccount.Namespace, role.Spec.ServiceAccount.Name),
		identityv1.LabelDelivery:       string(delivery),
	})

	return labels
}
