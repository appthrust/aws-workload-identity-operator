package controller

import "testing"

func TestInventoryNamespaceFromCluster(t *testing.T) {
	tests := []struct {
		clusterName string
		want        string
	}{
		{clusterName: testResolvedClusterName, want: testInventoryNamespace},
		{clusterName: testInventoryNamespace, want: ""},
		{clusterName: "/" + testInventoryNamespace, want: ""},
		{clusterName: testInventoryNamespace + "/", want: ""},
		{clusterName: "", want: ""},
	}

	for _, tt := range tests {
		if got := inventoryNamespaceFromCluster(tt.clusterName); got != tt.want {
			t.Fatalf("inventoryNamespaceFromCluster(%q) = %q, want %q", tt.clusterName, got, tt.want)
		}
	}
}
