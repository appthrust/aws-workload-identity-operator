package aws

import "testing"

func TestNormalizeIssuerURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "EKS issuer",
			raw:  "https://oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE",
			want: "oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE",
		},
		{
			name: "host only",
			raw:  "https://issuer.example.com",
			want: "issuer.example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeIssuerURL(tt.raw)
			if err != nil {
				t.Fatalf("NormalizeIssuerURL(%q): %v", tt.raw, err)
			}

			if got != tt.want {
				t.Fatalf("NormalizeIssuerURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestNormalizeIssuerURLRejectsNonCanonicalInput(t *testing.T) {
	for _, raw := range []string{
		"http://oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE",
		"https://oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE/",
		"https://oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE?x=y",
		"https://oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE#fragment",
		"https://user@oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE",
		"https://issuer.example.com\t/id/EXAMPLE",
		"https://",
	} {
		if got, err := NormalizeIssuerURL(raw); err == nil {
			t.Fatalf("expected %q to be rejected, got %q", raw, got)
		}
	}
}

func TestOIDCProviderARNIssuerHostPath(t *testing.T) {
	got, err := OIDCProviderARNIssuerHostPath("arn:aws:iam::123456789012:oidc-provider/oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE")
	if err != nil {
		t.Fatal(err)
	}

	if got != "oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE" {
		t.Fatalf("unexpected provider host/path %q", got)
	}
}

func TestValidateOIDCProviderARNMatchesIssuer(t *testing.T) {
	if err := ValidateOIDCProviderARNMatchesIssuer(
		"arn:aws:iam::123456789012:oidc-provider/oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE",
		"oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE",
	); err != nil {
		t.Fatal(err)
	}

	err := ValidateOIDCProviderARNMatchesIssuer(
		"arn:aws:iam::123456789012:oidc-provider/oidc.eks.ap-northeast-1.amazonaws.com/id/OTHER",
		"oidc.eks.ap-northeast-1.amazonaws.com/id/EXAMPLE",
	)
	if err == nil {
		t.Fatal("expected mismatched provider path to be rejected")
	}
}

func TestOIDCProviderARNIssuerHostPathRejectsInvalidARN(t *testing.T) {
	for _, arn := range []string{
		"arn:aws:iam::123456789012:role/example",
		"arn:aws:iam:us-east-1:123456789012:oidc-provider/example.com",
		"arn:aws:iam::abc:oidc-provider/example.com",
		"arn:aws:iam::123456789012:oidc-provider/https://issuer.example.com",
		"arn:aws:iam::123456789012:oidc-provider/issuer.example.com?x=y",
		"arn:aws:iam::123456789012:oidc-provider/issuer.example.com#fragment",
		"arn:aws:iam::123456789012:oidc-provider/issuer.example.com\t/id/EXAMPLE",
		"arn:aws:iam::123456789012:oidc-provider/issuer.example.com/id/EXAMPLE/",
		"not-an-arn",
	} {
		if got, err := OIDCProviderARNIssuerHostPath(arn); err == nil {
			t.Fatalf("expected %q to be rejected, got %q", arn, got)
		}
	}
}
