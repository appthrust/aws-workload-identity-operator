package main

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clusterinventoryv1alpha1 "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/cluster-inventory-api/pkg/access"
	"sigs.k8s.io/controller-runtime/pkg/client"

	identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"
	"github.com/appthrust/aws-workload-identity-operator/pkg/remoteirsa"
)

type remoteIRSAOptions = remoteirsa.Options

type dependencies struct {
	loadAccessConfig func(path string) (*access.Config, error)
	buildHubConfig   func(kubeconfig string) (*rest.Config, error)
	newHubClient     func(config *rest.Config) (client.Client, error)
	newHubKubeClient func(config *rest.Config) (kubernetes.Interface, error)
	newProvider      func(opts remoteIRSAOptions) (credentialProvider, error)
}

func productionDependencies() dependencies {
	return dependencies{
		loadAccessConfig: access.NewFromFile,
		buildHubConfig:   buildHubConfig,
		newHubClient:     newHubClient,
		newHubKubeClient: newHubKubeClient,
		newProvider: func(opts remoteIRSAOptions) (credentialProvider, error) {
			return remoteirsa.NewProvider(opts)
		},
	}
}

func buildHubConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("build kubeconfig from %q: %w", kubeconfig, err)
		}

		return config, nil
	}

	inClusterConfig, err := rest.InClusterConfig()
	if err == nil {
		return inClusterConfig, nil
	}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()

	config, loadingErr := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules,
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
	if loadingErr != nil {
		return nil, fmt.Errorf("in-cluster config unavailable and default kubeconfig failed: %w", loadingErr)
	}

	return config, nil
}

func newHubClient(config *rest.Config) (client.Client, error) {
	c, err := client.New(config, client.Options{Scheme: hubScheme()})
	if err != nil {
		return nil, fmt.Errorf("create controller-runtime client: %w", err)
	}

	return c, nil
}

func newHubKubeClient(config *rest.Config) (kubernetes.Interface, error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes clientset: %w", err)
	}

	return clientset, nil
}

func hubScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(identityv1.AddToScheme(scheme))
	utilruntime.Must(clusterinventoryv1alpha1.AddToScheme(scheme))

	return scheme
}
