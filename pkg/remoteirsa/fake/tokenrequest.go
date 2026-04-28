package fake

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
)

// TokenRequester is a test fake for remote ServiceAccount TokenRequest.
type TokenRequester struct {
	Calls []TokenRequestCall
	Token string
	Err   error
}

// TokenRequestCall records one TokenRequest call.
type TokenRequestCall struct {
	RestConfig     *rest.Config
	ServiceAccount types.NamespacedName
	Audience       string
	Expiration     time.Duration
}

// RequestServiceAccountToken records the call and returns the configured token or error.
func (r *TokenRequester) RequestServiceAccountToken(_ context.Context, restConfig *rest.Config, serviceAccount types.NamespacedName, audience string, expiration time.Duration) (string, error) {
	r.Calls = append(r.Calls, TokenRequestCall{
		RestConfig:     restConfig,
		ServiceAccount: serviceAccount,
		Audience:       audience,
		Expiration:     expiration,
	})
	if r.Err != nil {
		return "", r.Err
	}

	return r.Token, nil
}
