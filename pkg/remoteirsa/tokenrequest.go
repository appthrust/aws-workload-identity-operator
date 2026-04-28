package remoteirsa

import (
	"context"
	"fmt"
	"time"

	authv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// NewTokenRequester returns the default Kubernetes TokenRequest client.
func NewTokenRequester() TokenRequester {
	return tokenRequester{}
}

type tokenRequester struct{}

func (tokenRequester) RequestServiceAccountToken(ctx context.Context, restConfig *rest.Config, serviceAccount types.NamespacedName, audience string, expiration time.Duration) (string, error) {
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return "", fmt.Errorf("create Kubernetes client for remote TokenRequest: %w", err)
	}

	expirationSeconds := int64(expiration / time.Second)
	request := &authv1.TokenRequest{
		Spec: authv1.TokenRequestSpec{
			Audiences:         []string{audience},
			ExpirationSeconds: &expirationSeconds,
		},
	}

	response, err := clientset.CoreV1().
		ServiceAccounts(serviceAccount.Namespace).
		CreateToken(ctx, serviceAccount.Name, request, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("create remote ServiceAccount token: %w", err)
	}

	return response.Status.Token, nil
}

func classifyTokenRequestError(err error) error {
	if apierrors.IsForbidden(err) {
		return newError(ReasonRemoteTokenRequestForbidden, "remote ServiceAccount TokenRequest was forbidden", err, errorContext{})
	}

	return newError(ReasonRemoteTokenRequestFailed, "remote ServiceAccount TokenRequest failed", err, errorContext{})
}
