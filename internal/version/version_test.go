package version

import "testing"

func TestContractVersionIsThree(t *testing.T) {
	if ContractVersion != 3 {
		t.Fatalf("ContractVersion = %d, want 3 (must match tatara-operator and tatara-cli)", ContractVersion)
	}
}
