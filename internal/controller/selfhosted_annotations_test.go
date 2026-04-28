package controller

import "testing"

func TestRenderSelfHostedServiceAccountAnnotations(t *testing.T) {
	annotations := renderSelfHostedServiceAccountAnnotations("arn:aws:iam::123456789012:role/controller")

	if annotations[selfHostedRoleARNAnnotation] != "arn:aws:iam::123456789012:role/controller" {
		t.Fatalf("unexpected role annotation: %#v", annotations)
	}

	if annotations[selfHostedAudienceAnnotation] != "sts.amazonaws.com" ||
		annotations[selfHostedRegionalSTSAnnotation] != "true" ||
		annotations[selfHostedTokenExpirationAnnotation] != "86400" {
		t.Fatalf("unexpected webhook annotations: %#v", annotations)
	}
}

func TestRenderSelfHostedServiceAccountAnnotationsSkipsEmptyRoleARN(t *testing.T) {
	if annotations := renderSelfHostedServiceAccountAnnotations(""); len(annotations) != 0 {
		t.Fatalf("expected no annotations for empty role ARN, got %#v", annotations)
	}
}
