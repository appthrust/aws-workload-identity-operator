package aws

import (
	"encoding/json"
	"fmt"

	"k8s.io/apiserver/pkg/authentication/serviceaccount"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
	"github.com/appthrust/aws-workload-identity-operator/internal/oidc"
	"github.com/appthrust/aws-workload-identity-operator/pkg/remoteirsa"
)

type policyDocument struct {
	Version   string            `json:"Version"`
	Statement []policyStatement `json:"Statement"`
}

// policyStatement covers every IAM policy shape the operator emits. Principal
// is `any` because IAM accepts either a `"*"` wildcard or a `{Service|Federated: ...}`
// object; Action is `any` for the same reason (string vs []string).
type policyStatement struct {
	Sid       string         `json:"Sid,omitempty"`
	Effect    string         `json:"Effect"`
	Principal any            `json:"Principal,omitempty"`
	Action    any            `json:"Action"`
	Resource  []string       `json:"Resource,omitempty"`
	Condition map[string]any `json:"Condition,omitempty"`
}

// WebIdentityTrustPolicy renders an AssumeRoleWithWebIdentity trust policy for
// annotation-based IRSA deliveries.
func WebIdentityTrustPolicy(issuerHostPath, oidcProviderARN string, sa identityv1.ServiceAccountSubject) (string, error) {
	doc := policyDocument{
		Version: "2012-10-17",
		Statement: []policyStatement{{
			Effect:    "Allow",
			Principal: map[string]any{"Federated": oidcProviderARN},
			Action:    "sts:AssumeRoleWithWebIdentity",
			Condition: map[string]any{
				"StringEquals": map[string]string{
					issuerHostPath + ":aud": remoteirsa.STSAudience,
					issuerHostPath + ":sub": serviceaccount.MakeUsername(sa.Namespace, sa.Name),
				},
			},
		}},
	}

	return marshalPolicy(doc)
}

// EKSPodIdentityTrustPolicy renders an EKS Pod Identity trust policy.
func EKSPodIdentityTrustPolicy(eksClusterARN, awsOrganizationID string, sa identityv1.ServiceAccountSubject) (string, error) {
	equals := map[string]string{
		"aws:RequestTag/eks-cluster-arn":            eksClusterARN,
		"aws:RequestTag/kubernetes-namespace":       sa.Namespace,
		"aws:RequestTag/kubernetes-service-account": sa.Name,
	}
	if awsOrganizationID != "" {
		equals["aws:SourceOrgId"] = awsOrganizationID
	}

	doc := policyDocument{
		Version: "2012-10-17",
		Statement: []policyStatement{{
			Sid:       "AllowEksAuthToAssumeRoleForPodIdentity",
			Effect:    "Allow",
			Principal: map[string]any{"Service": EKSPodIdentityServicePrincipal},
			Action:    []string{"sts:AssumeRole", "sts:TagSession"},
			Condition: map[string]any{"StringEquals": equals},
		}},
	}

	return marshalPolicy(doc)
}

// BucketPolicy renders the public-read policy for OIDC discovery objects.
// Principal is the literal "*" wildcard (string form).
func BucketPolicy(bucket string) (string, error) {
	doc := policyDocument{
		Version: "2012-10-17",
		Statement: []policyStatement{{
			Sid:       "PublicReadOIDCDiscoveryOnly",
			Effect:    "Allow",
			Principal: "*",
			Action:    "s3:GetObject",
			Resource: []string{
				"arn:aws:s3:::" + bucket + "/" + oidc.DiscoveryObjectKey,
				"arn:aws:s3:::" + bucket + "/" + oidc.JWKSObjectKey,
			},
		}},
	}

	return marshalPolicy(doc)
}

func marshalPolicy(doc any) (string, error) {
	b, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("marshal policy: %w", err)
	}

	return string(b), nil
}
