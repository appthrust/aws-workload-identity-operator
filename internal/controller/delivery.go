package controller

import identityv1 "github.com/appthrust/aws-workload-identity-operator/api/v1alpha1"

func usesManagedConfigOIDCProvider(config *identityv1.AWSWorkloadIdentityConfig) bool {
	return config != nil &&
		config.Spec.Type == identityv1.DeliveryTypeEKSIRSA &&
		config.Spec.EKSIRSA != nil &&
		config.Spec.EKSIRSA.OIDCProvider.Management == identityv1.OIDCProviderManagementManaged
}
