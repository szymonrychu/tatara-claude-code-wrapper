package version

import "testing"

func TestContractVersionIsFour(t *testing.T) {
	if ContractVersion != 4 {
		t.Fatalf("ContractVersion = %d, want 4 (must match tatara-operator and tatara-cli)", ContractVersion)
	}
}
