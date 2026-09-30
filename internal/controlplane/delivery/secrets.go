package delivery

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/controlplane/authz"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/controlplane/secretkeys"
)

// sealedNamePattern restricts sealed names to environment variable identifiers.
var sealedNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

const MaxSealedNameLength = 128

// ValidateSealedSecretName rejects names that are not valid identifiers,
// collide with the reserved PLATFORM_ prefix, or exceed the length cap.
func ValidateSealedSecretName(name string) error {
	if len(name) == 0 || len(name) > MaxSealedNameLength || !sealedNamePattern.MatchString(name) {
		return fmt.Errorf("%w: %q must match [A-Za-z_][A-Za-z0-9_]* and be 1-%d characters",
			ErrInvalidSealedName, name, MaxSealedNameLength)
	}
	if strings.HasPrefix(name, "PLATFORM_") {
		return fmt.Errorf("%w: %q uses the reserved PLATFORM_ prefix", ErrInvalidSealedName, name)
	}
	return nil
}

// SealServiceSecret appends a new sealed version and reports its version. Names
// must be disjoint from public environment keys. Values are write-only: sealed
// here, never returned by any read.
func (d *Delivery) SealServiceSecret(ctx context.Context, user authz.User, serviceID, name string, value []byte) (int64, error) {
	scope, err := d.store.authz.AuthorizeService(ctx, user, serviceID, authz.Write)
	if err != nil {
		return 0, err
	}
	if err := ValidateSealedSecretName(name); err != nil {
		return 0, err
	}
	secrets := d.store.secrets
	if secrets == nil {
		return 0, ErrSealedSecretsUnavailable
	}
	var version int64
	err = d.store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		current, err := d.store.serviceByIDQuerier(ctx, tx, scope)
		if err != nil {
			return err
		}
		if _, ok := current.Spec.GetRuntime().GetEnv()[name]; ok {
			return fmt.Errorf("%w: %q is a public environment variable; remove it from the spec before sealing",
				ErrSealedNameConflict, name)
		}
		version, err = secrets.Sealed().Seal(ctx, tx, scope.ID(), scope.EnvironmentID(), name, value)
		if err != nil {
			return err
		}
		// Draft secret versions reach allocations when a deployment pins them.
		return nil
	})
	if err != nil {
		return 0, err
	}
	return version, nil
}

// DeleteServiceSecret tombstones a sealed secret. Pinned deployment reads keep
// resolving captured versions, so rollback still restores the secret.
func (d *Delivery) DeleteServiceSecret(ctx context.Context, user authz.User, serviceID, name string) error {
	scope, err := d.store.authz.AuthorizeService(ctx, user, serviceID, authz.Write)
	if err != nil {
		return err
	}
	secrets := d.store.secrets
	if secrets == nil {
		return ErrSealedSecretsUnavailable
	}
	return d.store.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := d.store.serviceByIDQuerier(ctx, tx, scope); err != nil {
			return err
		}
		if err := secrets.Sealed().Delete(ctx, tx, scope.ID(), name); err != nil {
			return err
		}
		return nil
	})
}

// ListServiceSecrets returns masked existence records for live secrets.
func (d *Delivery) ListServiceSecrets(ctx context.Context, user authz.User, serviceID string) ([]secretkeys.SecretMetadata, error) {
	scope, err := d.store.authz.AuthorizeService(ctx, user, serviceID, authz.Read)
	if err != nil {
		return nil, err
	}
	secrets := d.store.secrets
	if secrets == nil {
		return nil, ErrSealedSecretsUnavailable
	}
	metas, err := secrets.Sealed().ListMasked(ctx, d.store.db, scope.ID())
	if err != nil {
		return nil, err
	}
	return metas, nil
}

// rejectSealedNameConflicts fails spec updates colliding with sealed names.
func (d *Delivery) rejectSealedNameConflicts(ctx context.Context, tx *sql.Tx, serviceID string, publicEnv map[string]string) error {
	secrets := d.store.secrets
	if secrets == nil || len(publicEnv) == 0 {
		return nil
	}
	live, err := secrets.Sealed().CurrentVersions(ctx, tx, serviceID)
	if err != nil {
		return err
	}
	var conflicts []string
	for name := range publicEnv {
		if _, ok := live[name]; ok {
			conflicts = append(conflicts, name)
		}
	}
	if len(conflicts) == 0 {
		return nil
	}
	sort.Strings(conflicts)
	return fmt.Errorf("%w: %s sealed; delete the sealed secret before setting it as public",
		ErrSealedNameConflict, strings.Join(conflicts, ", "))
}

// resolveSealedEnv merges decrypted sealed values into an agent's desired
// state. It is the only control-plane path that decrypts, and only for services
// assigned to this agent. Each allocation uses only the sealed names and versions
// its deployment captured in the same immutable product prefix as its spec.
func (d *Delivery) resolveSealedEnv(ctx context.Context, product *journal.Projection, state *agentv1.DesiredNodeState) error {
	secrets := d.store.secrets
	if secrets == nil || state == nil {
		return nil
	}
	services := state.GetServices()
	if len(services) == 0 {
		return nil
	}
	deployments := make([]secretkeys.DeploymentSecrets, 0, len(services))
	seenDeployments := map[string]bool{}
	for _, svc := range services {
		id := svc.GetDeploymentId()
		if seenDeployments[id] {
			continue
		}
		deployment, exists := product.Deployments[id]
		if !exists || deployment.ServiceID != svc.GetServiceId() {
			return fmt.Errorf("allocation %s has no matching deployment %s in the captured product prefix", svc.GetAllocationId(), id)
		}
		var versions map[string]int64
		if err := json.Unmarshal(deployment.SealedVersionsJSON, &versions); err != nil {
			return fmt.Errorf("decode deployment %s sealed versions: %w", id, err)
		}
		seenDeployments[id] = true
		deployments = append(deployments, secretkeys.DeploymentSecrets{DeploymentID: id, ServiceID: deployment.ServiceID, Versions: versions})
	}
	resolved, err := secrets.Sealed().ResolveDeployments(ctx, d.store.db, deployments)
	if err != nil {
		return err
	}
	for _, svc := range services {
		values := resolved[svc.GetDeploymentId()]
		if len(values) == 0 {
			continue
		}
		if svc.GetSpec() == nil || svc.GetSpec().GetRuntime() == nil {
			continue
		}
		env := svc.GetSpec().GetRuntime().GetEnv()
		if env == nil {
			env = map[string]string{}
			svc.GetSpec().GetRuntime().Env = env
		}
		for name, value := range values {
			env[name] = value
		}
	}
	return nil
}
