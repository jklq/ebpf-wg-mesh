package productionops

import (
	"ebof-wg-mesh/internal/deploy"
	"testing"
)

func TestEveryDeclaredProofHasANativeInspection(t *testing.T) {
	for _, hook := range deploy.RequiredHooks() {
		contract, _ := deploy.ContractForHook(hook)
		for _, proof := range contract.Checks {
			if _, err := proofInspection(proof, hook); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := proofInspection("unimplemented-new-obligation", "production-verify"); err == nil {
		t.Fatal("unimplemented obligation was automatically acknowledged")
	}
}
