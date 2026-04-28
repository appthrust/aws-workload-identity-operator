package aws

import (
	"encoding/json"
	"testing"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
)

func firstStatement(t *testing.T, parsed map[string]any) map[string]any {
	t.Helper()

	statements, ok := parsed["Statement"].([]any)
	if !ok || len(statements) == 0 {
		t.Fatalf("statement list missing: %#v", parsed["Statement"])
	}

	statement, ok := statements[0].(map[string]any)
	if !ok {
		t.Fatalf("statement is not an object: %#v", statements[0])
	}

	return statement
}

func stringEquals(t *testing.T, statement map[string]any) map[string]any {
	t.Helper()

	condition, ok := statement["Condition"].(map[string]any)
	if !ok {
		t.Fatalf("condition is not an object: %#v", statement["Condition"])
	}

	equals, ok := condition["StringEquals"].(map[string]any)
	if !ok {
		t.Fatalf("StringEquals is not an object: %#v", condition["StringEquals"])
	}

	return equals
}

func TestWebIdentityTrustPolicyContainsAudAndSub(t *testing.T) {
	policy, err := WebIdentityTrustPolicy("bucket.s3.ap-northeast-1.amazonaws.com", "arn:aws:iam::123456789012:oidc-provider/bucket", identityv1.ServiceAccountSubject{Namespace: "kube-system", Name: "controller"})
	if err != nil {
		t.Fatal(err)
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(policy), &parsed); err != nil {
		t.Fatal(err)
	}

	statement := firstStatement(t, parsed)
	if got := statement["Action"]; got != "sts:AssumeRoleWithWebIdentity" {
		t.Fatalf("unexpected action: %v", got)
	}

	equals := stringEquals(t, statement)
	if equals["bucket.s3.ap-northeast-1.amazonaws.com:aud"] != "sts.amazonaws.com" {
		t.Fatalf("aud condition missing: %#v", equals)
	}

	if equals["bucket.s3.ap-northeast-1.amazonaws.com:sub"] != "system:serviceaccount:kube-system:controller" {
		t.Fatalf("sub condition missing: %#v", equals)
	}
}

func TestWebIdentityTrustPolicySupportsEKSIssuer(t *testing.T) {
	policy, err := WebIdentityTrustPolicy(
		"oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE",
		"arn:aws:iam::123456789012:oidc-provider/oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE",
		identityv1.ServiceAccountSubject{Namespace: "apps", Name: "api"},
	)
	if err != nil {
		t.Fatal(err)
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(policy), &parsed); err != nil {
		t.Fatal(err)
	}

	statement := firstStatement(t, parsed)
	principal, ok := statement["Principal"].(map[string]any)
	if !ok {
		t.Fatalf("principal is not an object: %#v", statement["Principal"])
	}

	if principal["Federated"] != "arn:aws:iam::123456789012:oidc-provider/oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE" {
		t.Fatalf("unexpected principal: %#v", principal)
	}

	equals := stringEquals(t, statement)
	if equals["oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE:aud"] != "sts.amazonaws.com" {
		t.Fatalf("aud condition missing: %#v", equals)
	}

	if equals["oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE:sub"] != "system:serviceaccount:apps:api" {
		t.Fatalf("sub condition missing: %#v", equals)
	}
}

func TestEKSPodIdentityTrustPolicyUsesRequestTags(t *testing.T) {
	policy, err := EKSPodIdentityTrustPolicy("arn:aws:eks:ap-northeast-1:123456789012:cluster/prod", "o-abc", identityv1.ServiceAccountSubject{Namespace: "apps", Name: "api"})
	if err != nil {
		t.Fatal(err)
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(policy), &parsed); err != nil {
		t.Fatal(err)
	}

	statement := firstStatement(t, parsed)

	principal, ok := statement["Principal"].(map[string]any)
	if !ok {
		t.Fatalf("principal is not an object: %#v", statement["Principal"])
	}

	if principal["Service"] != "pods.eks.amazonaws.com" {
		t.Fatalf("unexpected principal: %#v", principal)
	}

	equals := stringEquals(t, statement)
	if equals["aws:RequestTag/eks-cluster-arn"] == "" || equals["aws:SourceOrgId"] != "o-abc" {
		t.Fatalf("expected EKS and org conditions: %#v", equals)
	}
}
