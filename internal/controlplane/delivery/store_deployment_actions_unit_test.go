package delivery

import (
	"testing"
)

func TestDeploymentReusableForRollback(t *testing.T) {
	t.Parallel()
	if !deploymentReusableForRollback(DeploymentStateDraining) || !deploymentReusableForRollback(DeploymentStateCompleted) || !deploymentReusableForRollback(DeploymentStateRemoved) {
		t.Fatal("expected previously successful deployments to be rollback sources")
	}
	if deploymentReusableForRollback(DeploymentStateFailed) || deploymentReusableForRollback(DeploymentStateCancelled) {
		t.Fatal("failed/cancelled deployments must not be rollback sources")
	}
}
