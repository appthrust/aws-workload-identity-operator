package remoteirsacredentialprocess

import (
	"fmt"

	"sigs.k8s.io/cluster-inventory-api/pkg/access"
)

type AccessConfigOptions struct {
	ProviderFile string
}

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
