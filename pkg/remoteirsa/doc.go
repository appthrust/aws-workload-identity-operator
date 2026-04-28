// Package remoteirsa provides an AWS SDK v2 credentials provider for hub-side
// consumers that need AWS credentials for a ServiceAccount on a remote
// Kubernetes cluster.
//
// The package keeps Kubernetes cluster access and AWS credentials separate.
// Cluster Inventory access providers are used only to build a remote
// Kubernetes rest.Config. The provider then requests a fresh remote
// ServiceAccount token with audience sts.amazonaws.com and exchanges that token
// with AWS STS AssumeRoleWithWebIdentity.
//
// The provider never exposes Kubernetes web identity tokens and does not cache
// them. Wrap Provider with aws.CredentialsCache, or use NewCachedProvider, when
// AWS credential caching is desired.
package remoteirsa
