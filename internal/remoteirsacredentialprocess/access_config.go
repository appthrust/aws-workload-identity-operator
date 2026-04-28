// Package remoteirsacredentialprocess runs the AWS credential_process entrypoint
// for remote IRSA workloads.
package remoteirsacredentialprocess

import (
	"fmt"

	"sigs.k8s.io/cluster-inventory-api/pkg/access"
)

// AccessConfigOptions configures Cluster Inventory access provider loading.
type AccessConfigOptions struct {
	ProviderFile string
}

// BuildAccessConfig loads the Cluster Inventory access provider configuration.
func BuildAccessConfig(opts AccessConfigOptions) (*access.Config, error) {
	if opts.ProviderFile == "" {
		return nil, fmt.Errorf("clusterprofile provider file is required")
	}

	cfg, err := access.NewFromFile(opts.ProviderFile)
	if err != nil {
		return nil, fmt.Errorf("load clusterprofile provider file: %w", err)
	}

	return cfg, nil
}
