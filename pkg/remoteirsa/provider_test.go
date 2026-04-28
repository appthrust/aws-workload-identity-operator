package remoteirsa_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	ststypes "github.com/aws/aws-sdk-go-v2/service/sts/types"
	"github.com/aws/smithy-go"
	authv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	clientcmdv1 "k8s.io/client-go/tools/clientcmd/api/v1"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/cluster-inventory-api/pkg/access"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
	"github.com/appthrust/aws-workload-identity-operator/pkg/remoteirsa"
	remotefake "github.com/appthrust/aws-workload-identity-operator/pkg/remoteirsa/fake"
)

func TestRetrieveResolvesRoleByServiceAccountAndCallsTokenRequestAndSTS(t *testing.T) { //nolint:cyclop,funlen // End-to-end provider test intentionally checks the full credential request.
	ctx := context.Background()
	scheme := testScheme(t)
	reader := ctrlfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			testConfig("ap-northeast-1"),
			testRole("workload-role", "app", "arn:aws:iam::123456789012:role/workload"),
		).
		Build()
	tokenRequester := &remotefake.TokenRequester{Token: "jwt-token"}
	stsClient := &remotefake.STSClient{
		Output: &sts.AssumeRoleWithWebIdentityOutput{
			AssumedRoleUser: &ststypes.AssumedRoleUser{
				Arn: aws.String("arn:aws:sts::123456789012:assumed-role/workload/session"),
			},
			Credentials: &ststypes.Credentials{
				AccessKeyId:     aws.String("AKIAEXAMPLE"),
				SecretAccessKey: aws.String("secret"),
				SessionToken:    aws.String("session"),
				Expiration:      aws.Time(time.Now().Add(time.Hour).UTC()),
			},
		},
	}

	provider, err := remoteirsa.NewProvider(remoteirsa.Options{
		HubReader: reader,
		RemoteConfigResolver: staticRemoteConfigResolver{
			cfg: &rest.Config{Host: "https://remote.example.com"},
			profile: remoteirsa.ResolvedClusterProfile{Ref: types.NamespacedName{
				Namespace: "inventory",
				Name:      "wlc-a",
			}, ProviderName: "open-cluster-management"},
		},
		TokenRequester:    tokenRequester,
		WorkloadNamespace: "wlc-a",
		ServiceAccount: types.NamespacedName{
			Namespace: "app",
			Name:      "workload",
		},
		TokenExpiration: 20 * time.Minute,
		SessionDuration: 30 * time.Minute,
		SessionName:     "app-workload",
		STSClientFactory: func(region string) remoteirsa.STSAssumeRoleWithWebIdentityAPI {
			if region != "ap-northeast-1" {
				t.Fatalf("STS region = %q, want ap-northeast-1", region)
			}

			return stsClient
		},
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	creds, err := provider.Retrieve(ctx)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if creds.AccessKeyID != "AKIAEXAMPLE" || creds.SecretAccessKey != "secret" || creds.SessionToken != "session" {
		t.Fatalf("unexpected credentials: %#v", creds)
	}

	if !creds.CanExpire || creds.AccountID != "123456789012" {
		t.Fatalf("unexpected credential metadata: %#v", creds)
	}

	if len(tokenRequester.Calls) != 1 {
		t.Fatalf("TokenRequest calls = %d, want 1", len(tokenRequester.Calls))
	}

	if got := tokenRequester.Calls[0].Audience; got != remoteirsa.STSAudience {
		t.Fatalf("TokenRequest audience = %q, want %q", got, remoteirsa.STSAudience)
	}

	if got := tokenRequester.Calls[0].ServiceAccount; got != (types.NamespacedName{Namespace: "app", Name: "workload"}) {
		t.Fatalf("TokenRequest service account = %s", got)
	}

	if len(stsClient.Calls) != 1 {
		t.Fatalf("STS calls = %d, want 1", len(stsClient.Calls))
	}

	input := stsClient.Calls[0].Input
	if got := aws.ToString(input.WebIdentityToken); got != "jwt-token" {
		t.Fatalf("STS token = %q, want jwt-token", got)
	}

	if got := aws.ToString(input.RoleSessionName); got != "app-workload" {
		t.Fatalf("STS session name = %q, want app-workload", got)
	}

	if got := aws.ToInt32(input.DurationSeconds); got != int32((30 * time.Minute).Seconds()) {
		t.Fatalf("STS duration = %d", got)
	}
}

func TestRetrieveAcceptsEKSIRSA(t *testing.T) {
	ctx := context.Background()
	reader := ctrlfake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			testConfigWithType(identityv1.DeliveryTypeEKSIRSA, "ap-northeast-1"),
			testRole("workload-role", "app", "arn:aws:iam::123456789012:role/workload"),
		).
		Build()
	tokenRequester := &remotefake.TokenRequester{Token: "jwt-token"}
	stsClient := &remotefake.STSClient{
		Output: &sts.AssumeRoleWithWebIdentityOutput{
			Credentials: &ststypes.Credentials{
				AccessKeyId:     aws.String("AKIAEKSIRSA"),
				SecretAccessKey: aws.String("secret"),
				SessionToken:    aws.String("session"),
				Expiration:      aws.Time(time.Now().Add(time.Hour).UTC()),
			},
		},
	}

	provider, err := remoteirsa.NewProvider(remoteirsa.Options{
		HubReader: reader,
		RemoteConfigResolver: staticRemoteConfigResolver{
			cfg: &rest.Config{Host: "https://remote.example.com"},
			profile: remoteirsa.ResolvedClusterProfile{Ref: types.NamespacedName{
				Namespace: "inventory",
				Name:      "wlc-a",
			}, ProviderName: "open-cluster-management"},
		},
		TokenRequester:    tokenRequester,
		WorkloadNamespace: "wlc-a",
		ServiceAccount:    types.NamespacedName{Namespace: "app", Name: "workload"},
		SessionName:       "app-workload",
		STSClientFactory: func(region string) remoteirsa.STSAssumeRoleWithWebIdentityAPI {
			if region != "ap-northeast-1" {
				t.Fatalf("STS region = %q, want ap-northeast-1", region)
			}

			return stsClient
		},
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	creds, err := provider.Retrieve(ctx)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if creds.AccessKeyID != "AKIAEKSIRSA" {
		t.Fatalf("unexpected AccessKeyID %q", creds.AccessKeyID)
	}
	if len(tokenRequester.Calls) != 1 {
		t.Fatalf("TokenRequest calls = %d, want 1", len(tokenRequester.Calls))
	}
}

func TestRetrieveRejectsEKSPodIdentity(t *testing.T) {
	reader := ctrlfake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			testConfigWithType(identityv1.DeliveryTypeEKSPodIdentity, "ap-northeast-1"),
			testRole("workload-role", "app", "arn:aws:iam::123456789012:role/workload"),
		).
		Build()
	tokenRequester := &remotefake.TokenRequester{Token: "jwt-token"}

	provider, err := remoteirsa.NewProvider(remoteirsa.Options{
		HubReader:            reader,
		RemoteConfigResolver: staticRemoteConfigResolver{cfg: &rest.Config{Host: "https://remote.example.com"}},
		TokenRequester:       tokenRequester,
		WorkloadNamespace:    "wlc-a",
		ServiceAccount:       types.NamespacedName{Namespace: "app", Name: "workload"},
		SessionName:          "app-workload",
		STSClientFactory: func(_ string) remoteirsa.STSAssumeRoleWithWebIdentityAPI {
			t.Fatal("STSClientFactory must not be called for EKSPodIdentity")

			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	_, err = provider.Retrieve(context.Background())
	if got := remoteirsa.Reason(err); got != remoteirsa.ReasonUnsupportedDeliveryType {
		t.Fatalf("Reason = %q, want %q; err=%v", got, remoteirsa.ReasonUnsupportedDeliveryType, err)
	}
	if len(tokenRequester.Calls) != 0 {
		t.Fatalf("TokenRequest calls = %d, want 0", len(tokenRequester.Calls))
	}
}

func TestRetrieveRejectsUnsupportedResolvedDeliveryType(t *testing.T) {
	tokenRequester := &remotefake.TokenRequester{Token: "jwt-token"}
	remoteResolver := &recordingRemoteConfigResolver{}
	stsFactoryCalled := false

	provider, err := remoteirsa.NewProvider(remoteirsa.Options{
		HubResolver: staticHubResolver{role: remoteirsa.ResolvedRole{
			WorkloadNamespace: "wlc-a",
			ConfigRef:         types.NamespacedName{Namespace: "wlc-a", Name: identityv1.DefaultName},
			RoleRef:           types.NamespacedName{Namespace: "wlc-a", Name: "workload-role"},
			ServiceAccount:    types.NamespacedName{Namespace: "app", Name: "workload"},
			RoleARN:           "arn:aws:iam::123456789012:role/workload",
			Region:            "ap-northeast-1",
			DeliveryType:      string(identityv1.DeliveryTypeEKSPodIdentity),
		}},
		RemoteConfigResolver: remoteResolver,
		TokenRequester:       tokenRequester,
		WorkloadNamespace:    "wlc-a",
		ServiceAccount:       types.NamespacedName{Namespace: "app", Name: "workload"},
		SessionName:          "app-workload",
		STSClientFactory: func(_ string) remoteirsa.STSAssumeRoleWithWebIdentityAPI {
			stsFactoryCalled = true
			t.Fatal("STSClientFactory must not be called for unsupported resolved delivery type")

			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	_, err = provider.Retrieve(context.Background())
	if got := remoteirsa.Reason(err); got != remoteirsa.ReasonUnsupportedDeliveryType {
		t.Fatalf("Reason = %q, want %q; err=%v", got, remoteirsa.ReasonUnsupportedDeliveryType, err)
	}
	if remoteResolver.calls != 0 {
		t.Fatalf("RemoteConfigResolver calls = %d, want 0", remoteResolver.calls)
	}
	if len(tokenRequester.Calls) != 0 {
		t.Fatalf("TokenRequest calls = %d, want 0", len(tokenRequester.Calls))
	}
	if stsFactoryCalled {
		t.Fatal("STSClientFactory was called")
	}
}

func TestRetrievePrefersExplicitThenClusterProfileRegion(t *testing.T) { //nolint:funlen // Table test keeps region precedence cases together.
	tests := []struct {
		name                 string
		explicitRegion       string
		clusterProfileRegion string
		configRegion         string
		wantRegion           string
	}{
		{
			name:                 "cluster profile region overrides config region",
			clusterProfileRegion: "us-east-1",
			configRegion:         "ap-northeast-1",
			wantRegion:           "us-east-1",
		},
		{
			name:                 "explicit region overrides cluster profile region",
			explicitRegion:       "eu-west-1",
			clusterProfileRegion: "us-east-1",
			configRegion:         "ap-northeast-1",
			wantRegion:           "eu-west-1",
		},
		{
			name:         "config region is fallback",
			configRegion: "ap-northeast-1",
			wantRegion:   "ap-northeast-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := ctrlfake.NewClientBuilder().
				WithScheme(testScheme(t)).
				WithObjects(
					testConfig(tt.configRegion),
					testRole("workload-role", "app", "arn:aws:iam::123456789012:role/workload"),
				).
				Build()
			stsClient := &remotefake.STSClient{
				Output: &sts.AssumeRoleWithWebIdentityOutput{
					Credentials: &ststypes.Credentials{
						AccessKeyId:     aws.String("AKIAREGION"),
						SecretAccessKey: aws.String("secret"),
						SessionToken:    aws.String("session"),
						Expiration:      aws.Time(time.Now().Add(time.Hour).UTC()),
					},
				},
			}

			provider, err := remoteirsa.NewProvider(remoteirsa.Options{
				HubReader: reader,
				RemoteConfigResolver: staticRemoteConfigResolver{
					cfg: &rest.Config{Host: "https://remote.example.com"},
					profile: remoteirsa.ResolvedClusterProfile{
						Ref:       types.NamespacedName{Namespace: "inventory", Name: "wlc-a"},
						AWSRegion: tt.clusterProfileRegion,
					},
				},
				TokenRequester:    &remotefake.TokenRequester{Token: "jwt-token"},
				WorkloadNamespace: "wlc-a",
				ServiceAccount:    types.NamespacedName{Namespace: "app", Name: "workload"},
				Region:            tt.explicitRegion,
				SessionName:       "app-workload",
				STSClientFactory: func(region string) remoteirsa.STSAssumeRoleWithWebIdentityAPI {
					if region != tt.wantRegion {
						t.Fatalf("STS region = %q, want %q", region, tt.wantRegion)
					}

					return stsClient
				},
			})
			if err != nil {
				t.Fatalf("NewProvider: %v", err)
			}

			if _, err := provider.Retrieve(context.Background()); err != nil {
				t.Fatalf("Retrieve: %v", err)
			}

			if len(stsClient.Calls) != 1 {
				t.Fatalf("STS calls = %d, want 1", len(stsClient.Calls))
			}
		})
	}
}

func TestRetrieveReturnsTypedRoleARNNotReady(t *testing.T) {
	ctx := context.Background()
	reader := ctrlfake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			testConfig("us-west-2"),
			testRole("workload-role", "app", ""),
		).
		Build()

	provider, err := remoteirsa.NewProvider(remoteirsa.Options{
		HubReader:            reader,
		RemoteConfigResolver: staticRemoteConfigResolver{cfg: &rest.Config{Host: "https://remote.example.com"}},
		TokenRequester:       &remotefake.TokenRequester{Token: "jwt-token"},
		WorkloadNamespace:    "wlc-a",
		ServiceAccount:       types.NamespacedName{Namespace: "app", Name: "workload"},
		SessionName:          "app-workload",
		STSClientFactory: func(_ string) remoteirsa.STSAssumeRoleWithWebIdentityAPI {
			t.Fatal("STSClientFactory must not be called when roleARN is not ready")

			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	_, err = provider.Retrieve(ctx)
	if got := remoteirsa.Reason(err); got != remoteirsa.ReasonRoleARNNotReady {
		t.Fatalf("Reason = %q, want %q; err=%v", got, remoteirsa.ReasonRoleARNNotReady, err)
	}

	if !remoteirsa.Temporary(err) {
		t.Fatalf("Temporary = false, want true")
	}
}

func TestRetrieveReturnsTypedRoleARNNotReadyWhenStatusStale(t *testing.T) {
	ctx := context.Background()
	role := testRole("workload-role", "app", "arn:aws:iam::123456789012:role/workload")
	role.Generation = 2
	role.Status.ObservedGeneration = 1
	reader := ctrlfake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			testConfig("us-west-2"),
			role,
		).
		Build()

	provider, err := remoteirsa.NewProvider(remoteirsa.Options{
		HubReader:            reader,
		RemoteConfigResolver: staticRemoteConfigResolver{cfg: &rest.Config{Host: "https://remote.example.com"}},
		TokenRequester:       &remotefake.TokenRequester{Token: "jwt-token"},
		WorkloadNamespace:    "wlc-a",
		ServiceAccount:       types.NamespacedName{Namespace: "app", Name: "workload"},
		SessionName:          "app-workload",
		STSClientFactory: func(_ string) remoteirsa.STSAssumeRoleWithWebIdentityAPI {
			t.Fatal("STSClientFactory must not be called when role status is stale")

			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	_, err = provider.Retrieve(ctx)
	if got := remoteirsa.Reason(err); got != remoteirsa.ReasonRoleARNNotReady {
		t.Fatalf("Reason = %q, want %q; err=%v", got, remoteirsa.ReasonRoleARNNotReady, err)
	}

	if !remoteirsa.Temporary(err) {
		t.Fatalf("Temporary = false, want true")
	}
}

func TestRetrieveReturnsTypedRoleARNNotReadyWhenReadyMissing(t *testing.T) {
	ctx := context.Background()
	role := testRole("workload-role", "app", "arn:aws:iam::123456789012:role/workload")
	role.Generation = 1
	role.Status.ObservedGeneration = 1
	role.Status.Conditions = nil
	reader := ctrlfake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			testConfig("us-west-2"),
			role,
		).
		Build()

	provider, err := remoteirsa.NewProvider(remoteirsa.Options{
		HubReader:            reader,
		RemoteConfigResolver: staticRemoteConfigResolver{cfg: &rest.Config{Host: "https://remote.example.com"}},
		TokenRequester:       &remotefake.TokenRequester{Token: "jwt-token"},
		WorkloadNamespace:    "wlc-a",
		ServiceAccount:       types.NamespacedName{Namespace: "app", Name: "workload"},
		SessionName:          "app-workload",
		STSClientFactory: func(_ string) remoteirsa.STSAssumeRoleWithWebIdentityAPI {
			t.Fatal("STSClientFactory must not be called when Ready condition is missing")

			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	_, err = provider.Retrieve(ctx)
	if got := remoteirsa.Reason(err); got != remoteirsa.ReasonRoleARNNotReady {
		t.Fatalf("Reason = %q, want %q; err=%v", got, remoteirsa.ReasonRoleARNNotReady, err)
	}
}

func TestRetrieveReturnsTypedRoleARNNotReadyWhenReadyFalse(t *testing.T) {
	ctx := context.Background()
	role := testRole("workload-role", "app", "arn:aws:iam::123456789012:role/workload")
	role.Generation = 1
	role.Status.ObservedGeneration = 1
	role.Status.Conditions = []metav1.Condition{{
		Type:               identityv1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             "NotReady",
		Message:            "not ready",
		LastTransitionTime: metav1.Now(),
	}}
	reader := ctrlfake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			testConfig("us-west-2"),
			role,
		).
		Build()

	provider, err := remoteirsa.NewProvider(remoteirsa.Options{
		HubReader:            reader,
		RemoteConfigResolver: staticRemoteConfigResolver{cfg: &rest.Config{Host: "https://remote.example.com"}},
		TokenRequester:       &remotefake.TokenRequester{Token: "jwt-token"},
		WorkloadNamespace:    "wlc-a",
		ServiceAccount:       types.NamespacedName{Namespace: "app", Name: "workload"},
		SessionName:          "app-workload",
		STSClientFactory: func(_ string) remoteirsa.STSAssumeRoleWithWebIdentityAPI {
			t.Fatal("STSClientFactory must not be called when Ready condition is False")

			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	_, err = provider.Retrieve(ctx)
	if got := remoteirsa.Reason(err); got != remoteirsa.ReasonRoleARNNotReady {
		t.Fatalf("Reason = %q, want %q; err=%v", got, remoteirsa.ReasonRoleARNNotReady, err)
	}
}

func TestRetrieveReturnsTypedRoleARNNotReadyWhenReadyConditionStale(t *testing.T) {
	ctx := context.Background()
	role := testRole("workload-role", "app", "arn:aws:iam::123456789012:role/workload")
	role.Generation = 2
	role.Status.ObservedGeneration = 2
	role.Status.Conditions = []metav1.Condition{{
		Type:               identityv1.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             identityv1.ReasonReady,
		Message:            "ready",
		ObservedGeneration: 1,
		LastTransitionTime: metav1.Now(),
	}}
	reader := ctrlfake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			testConfig("us-west-2"),
			role,
		).
		Build()

	provider, err := remoteirsa.NewProvider(remoteirsa.Options{
		HubReader:            reader,
		RemoteConfigResolver: staticRemoteConfigResolver{cfg: &rest.Config{Host: "https://remote.example.com"}},
		TokenRequester:       &remotefake.TokenRequester{Token: "jwt-token"},
		WorkloadNamespace:    "wlc-a",
		ServiceAccount:       types.NamespacedName{Namespace: "app", Name: "workload"},
		SessionName:          "app-workload",
		STSClientFactory: func(_ string) remoteirsa.STSAssumeRoleWithWebIdentityAPI {
			t.Fatal("STSClientFactory must not be called when Ready condition is stale")

			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	_, err = provider.Retrieve(ctx)
	if got := remoteirsa.Reason(err); got != remoteirsa.ReasonRoleARNNotReady {
		t.Fatalf("Reason = %q, want %q; err=%v", got, remoteirsa.ReasonRoleARNNotReady, err)
	}
}

func TestRetrieveResolvesExplicitRoleName(t *testing.T) {
	ctx := context.Background()
	reader := ctrlfake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			testConfig("us-west-2"),
			testRole("workload-role", "app", "arn:aws:iam::123456789012:role/workload"),
			testRole("other-role", "other", "arn:aws:iam::123456789012:role/other"),
		).
		Build()
	stsClient := &remotefake.STSClient{
		Output: &sts.AssumeRoleWithWebIdentityOutput{
			Credentials: &ststypes.Credentials{
				AccessKeyId:     aws.String("AKIAEXPLICIT"),
				SecretAccessKey: aws.String("secret"),
				SessionToken:    aws.String("session"),
				Expiration:      aws.Time(time.Now().Add(time.Hour).UTC()),
			},
		},
	}

	provider, err := remoteirsa.NewProvider(remoteirsa.Options{
		HubReader:                 reader,
		RemoteConfigResolver:      staticRemoteConfigResolver{cfg: &rest.Config{Host: "https://remote.example.com"}},
		TokenRequester:            &remotefake.TokenRequester{Token: "jwt-token"},
		WorkloadNamespace:         "wlc-a",
		AWSServiceAccountRoleName: "workload-role",
		ServiceAccount:            types.NamespacedName{Namespace: "app", Name: "workload"},
		SessionName:               "app-workload",
		STSClientFactory: func(_ string) remoteirsa.STSAssumeRoleWithWebIdentityAPI {
			return stsClient
		},
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	creds, err := provider.Retrieve(ctx)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}

	if creds.AccessKeyID != "AKIAEXPLICIT" {
		t.Fatalf("AccessKeyID = %q", creds.AccessKeyID)
	}

	if got := aws.ToString(stsClient.Calls[0].Input.RoleArn); got != "arn:aws:iam::123456789012:role/workload" {
		t.Fatalf("RoleArn = %q", got)
	}
}

func TestRetrieveClassifiesTokenRequestForbidden(t *testing.T) {
	ctx := context.Background()
	reader := ctrlfake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			testConfig("us-west-2"),
			testRole("workload-role", "app", "arn:aws:iam::123456789012:role/workload"),
		).
		Build()
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "", Resource: "serviceaccounts/token"}, "workload", errors.New("denied"))

	provider, err := remoteirsa.NewProvider(remoteirsa.Options{
		HubReader:            reader,
		RemoteConfigResolver: staticRemoteConfigResolver{cfg: &rest.Config{Host: "https://remote.example.com"}},
		TokenRequester:       &remotefake.TokenRequester{Err: forbidden},
		WorkloadNamespace:    "wlc-a",
		ServiceAccount:       types.NamespacedName{Namespace: "app", Name: "workload"},
		SessionName:          "app-workload",
		STSClientFactory: func(_ string) remoteirsa.STSAssumeRoleWithWebIdentityAPI {
			t.Fatal("STSClientFactory must not be called when TokenRequest fails")

			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	_, err = provider.Retrieve(ctx)
	if got := remoteirsa.Reason(err); got != remoteirsa.ReasonRemoteTokenRequestForbidden {
		t.Fatalf("Reason = %q, want %q; err=%v", got, remoteirsa.ReasonRemoteTokenRequestForbidden, err)
	}
}

func TestRetrieveClassifiesSTSAccessDenied(t *testing.T) {
	ctx := context.Background()
	reader := ctrlfake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			testConfig("us-west-2"),
			testRole("workload-role", "app", "arn:aws:iam::123456789012:role/workload"),
		).
		Build()
	stsClient := &remotefake.STSClient{Err: &smithy.GenericAPIError{Code: "AccessDenied", Message: "denied"}}

	provider, err := remoteirsa.NewProvider(remoteirsa.Options{
		HubReader:            reader,
		RemoteConfigResolver: staticRemoteConfigResolver{cfg: &rest.Config{Host: "https://remote.example.com"}},
		TokenRequester:       &remotefake.TokenRequester{Token: "jwt-token"},
		WorkloadNamespace:    "wlc-a",
		ServiceAccount:       types.NamespacedName{Namespace: "app", Name: "workload"},
		SessionName:          "app-workload",
		STSClientFactory: func(_ string) remoteirsa.STSAssumeRoleWithWebIdentityAPI {
			return stsClient
		},
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}

	_, err = provider.Retrieve(ctx)
	if got := remoteirsa.Reason(err); got != remoteirsa.ReasonSTSAccessDenied {
		t.Fatalf("Reason = %q, want %q; err=%v", got, remoteirsa.ReasonSTSAccessDenied, err)
	}
}

func TestRemoteConfigResolverUsesOCMLabelAndAccessProvidersOnly(t *testing.T) {
	ctx := context.Background()
	legacyHost := "https://legacy.example.com"
	ocmHost := "https://ocm.example.com"
	reader := ctrlfake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(&clusterinventoryv1alpha1.ClusterProfile{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "inventory",
				Name:      "wlc-a",
				Labels: map[string]string{
					"open-cluster-management.io/cluster-name":       "wlc-a",
					clusterinventoryv1alpha1.LabelClusterManagerKey: "open-cluster-management",
				},
			},
			Status: clusterinventoryv1alpha1.ClusterProfileStatus{
				Properties: []clusterinventoryv1alpha1.Property{{
					Name:  remoteirsa.PropertyAWSRegion,
					Value: "us-east-1",
				}},
				CredentialProviders: []clusterinventoryv1alpha1.CredentialProvider{{
					Name:    "legacy",
					Cluster: clientcmdv1Cluster(legacyHost),
				}},
				AccessProviders: []clusterinventoryv1alpha1.AccessProvider{{
					Name:    "ocm",
					Cluster: clientcmdv1Cluster(ocmHost),
				}},
			},
		}).
		Build()
	resolver := remoteirsa.NewRemoteConfigResolver(reader)
	accessConfig := access.New([]access.Provider{
		testAccessProvider("legacy"),
		testAccessProvider("ocm"),
	})

	cfg, profile, err := resolver.ResolveRemoteConfig(ctx, "wlc-a", accessConfig)
	if err != nil {
		t.Fatalf("ResolveRemoteConfig: %v", err)
	}

	if cfg.Host != ocmHost {
		t.Fatalf("resolved Host = %q, want %q", cfg.Host, ocmHost)
	}

	if profile.ProviderName != "ocm" {
		t.Fatalf("provider name = %q, want ocm", profile.ProviderName)
	}

	if profile.AWSRegion != "us-east-1" {
		t.Fatalf("AWSRegion = %q, want us-east-1", profile.AWSRegion)
	}

	if profile.Ref != (types.NamespacedName{Namespace: "inventory", Name: "wlc-a"}) {
		t.Fatalf("profile ref = %s", profile.Ref)
	}
}

func TestRemoteConfigResolverIgnoresSlashFormAWSRegionProperty(t *testing.T) {
	ctx := context.Background()
	reader := ctrlfake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(&clusterinventoryv1alpha1.ClusterProfile{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "inventory",
				Name:      "wlc-a",
				Labels: map[string]string{
					"open-cluster-management.io/cluster-name":       "wlc-a",
					clusterinventoryv1alpha1.LabelClusterManagerKey: "open-cluster-management",
				},
			},
			Status: clusterinventoryv1alpha1.ClusterProfileStatus{
				Properties: []clusterinventoryv1alpha1.Property{{
					Name:  "aws.identity.appthrust.io/aws-region",
					Value: "us-west-2",
				}},
				AccessProviders: []clusterinventoryv1alpha1.AccessProvider{{
					Name:    "ocm",
					Cluster: clientcmdv1Cluster("https://ocm.example.com"),
				}},
			},
		}).
		Build()
	resolver := remoteirsa.NewRemoteConfigResolver(reader)
	accessConfig := access.New([]access.Provider{testAccessProvider("ocm")})

	_, profile, err := resolver.ResolveRemoteConfig(ctx, "wlc-a", accessConfig)
	if err != nil {
		t.Fatalf("ResolveRemoteConfig: %v", err)
	}

	if profile.AWSRegion != "" {
		t.Fatalf("AWSRegion = %q, want empty because slash-form property is not supported", profile.AWSRegion)
	}
}

func TestTokenRequesterCreatesServiceAccountToken(t *testing.T) {
	ctx := context.Background()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}

		if r.URL.Path != "/api/v1/namespaces/app/serviceaccounts/workload/token" {
			t.Fatalf("path = %s", r.URL.Path)
		}

		var request authv1.TokenRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		if got := request.Spec.Audiences; len(got) != 1 || got[0] != remoteirsa.STSAudience {
			t.Fatalf("audiences = %#v", got)
		}

		if request.Spec.ExpirationSeconds == nil || *request.Spec.ExpirationSeconds != 900 {
			t.Fatalf("expirationSeconds = %#v, want 900", request.Spec.ExpirationSeconds)
		}

		w.Header().Set("Content-Type", "application/json")

		if err := json.NewEncoder(w).Encode(&authv1.TokenRequest{Status: authv1.TokenRequestStatus{Token: "remote-jwt"}}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer server.Close()

	requester := remoteirsa.NewTokenRequester()

	token, err := requester.RequestServiceAccountToken(ctx, &rest.Config{
		Host:          server.URL,
		ContentConfig: rest.ContentConfig{ContentType: "application/json"},
	}, types.NamespacedName{Namespace: "app", Name: "workload"}, remoteirsa.STSAudience, 15*time.Minute)
	if err != nil {
		t.Fatalf("RequestServiceAccountToken: %v", err)
	}

	if token != "remote-jwt" {
		t.Fatalf("token = %q, want remote-jwt", token)
	}
}

type staticRemoteConfigResolver struct {
	cfg     *rest.Config
	profile remoteirsa.ResolvedClusterProfile
	err     error
}

func (r staticRemoteConfigResolver) ResolveRemoteConfig(_ context.Context, _ string, _ *access.Config) (*rest.Config, remoteirsa.ResolvedClusterProfile, error) { //nolint:gocritic // Test fake keeps value semantics for inline literals.
	return r.cfg, r.profile, r.err
}

type recordingRemoteConfigResolver struct {
	calls int
}

func (r *recordingRemoteConfigResolver) ResolveRemoteConfig(_ context.Context, _ string, _ *access.Config) (*rest.Config, remoteirsa.ResolvedClusterProfile, error) {
	r.calls++

	return &rest.Config{Host: "https://unexpected.example.com"}, remoteirsa.ResolvedClusterProfile{}, nil
}

type staticHubResolver struct {
	role remoteirsa.ResolvedRole
	err  error
}

func (r staticHubResolver) Resolve(_ context.Context, _ remoteirsa.ResolveOptions) (remoteirsa.ResolvedRole, error) {
	return r.role, r.err
}

func testConfig(region string) *identityv1.AWSWorkloadIdentityConfig {
	return testConfigWithType(identityv1.DeliveryTypeSelfHostedIRSA, region)
}

func testConfigWithType(delivery identityv1.DeliveryType, region string) *identityv1.AWSWorkloadIdentityConfig {
	return &identityv1.AWSWorkloadIdentityConfig{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "wlc-a",
			Name:      identityv1.DefaultName,
		},
		Spec: identityv1.AWSWorkloadIdentityConfigSpec{
			Type:   delivery,
			Region: region,
		},
	}
}

func testRole(name, saNamespace, roleARN string) *identityv1.AWSServiceAccountRole {
	return &identityv1.AWSServiceAccountRole{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "wlc-a",
			Name:      name,
		},
		Spec: identityv1.AWSServiceAccountRoleSpec{
			ServiceAccount: identityv1.ServiceAccountSubject{
				Namespace: saNamespace,
				Name:      "workload",
			},
		},
		Status: identityv1.AWSServiceAccountRoleStatus{
			RoleARN: roleARN,
			Conditions: []metav1.Condition{{
				Type:               identityv1.ConditionReady,
				Status:             metav1.ConditionTrue,
				Reason:             identityv1.ReasonReady,
				Message:            "ready",
				LastTransitionTime: metav1.Now(),
			}},
		},
	}
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := identityv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add identity scheme: %v", err)
	}

	if err := clusterinventoryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add cluster inventory scheme: %v", err)
	}

	return scheme
}

func testAccessProvider(name string) access.Provider {
	return access.Provider{
		Name: name,
		ExecConfig: &clientcmdapi.ExecConfig{
			APIVersion: "client.authentication.k8s.io/v1beta1",
			Command:    "test-command",
		},
	}
}

func clientcmdv1Cluster(host string) clientcmdv1.Cluster {
	return clientcmdv1.Cluster{Server: host}
}
