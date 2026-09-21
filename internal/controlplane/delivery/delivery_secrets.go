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

// sealedNamePattern restricts sealed names to conventional environment
// variable identifiers.
var sealedNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// MaxSealedNameLength caps sealed secret names.
const MaxSealedNameLength = 128

// ValidateSealedSecretName rejects names that are not valid environment
// identifiers, that collide with the platform's reserved PLATFORM_ prefix,
// or that exceed the length cap.
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

// SealServiceSecret appends a new sealed version for a service's secret and
// reports its version. Names must be disjoint from the service's public
// environment keys: sealing a name that exists in the current spec fails,
// and updating the spec with a sealed name fails. Values are write-only:
// they are sealed into ciphertext here and never returned by any read.
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
		// Sealed rows are not journaled (ciphertext is not product state
		// the live view replays), but agents must still re-pull desired
		// state to pick up the new value.
		journal.RecordService(ctx, scope.ID())
		return nil
	})
	if err != nil {
		return 0, err
	}
	return version, nil
}

// DeleteServiceSecret tombstones a sealed secret so current resolution
// skips it. Pinned deployment reads keep resolving captured versions, so a
// rollback to a deployment that captured the secret still restores it.
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
		journal.RecordService(ctx, scope.ID())
		return nil
	})
}

// ListServiceSecrets returns masked existence records (names and versions,
// never values) for a service's live secrets.
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

// rejectSealedNameConflicts fails a public spec update whose environment
// keys collide with the service's live sealed names.
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

// resolveSealedEnv merges decrypted sealed values into an agent's already
// assembled desired state. It is the only control-plane path that decrypts
// sealed values, and it decrypts exactly the services assigned to this
// agent. Deployments pin sealed versions: a service resolves the versions
// captured by its assignment's deployment, falling back to current for
// names the deployment predates.
func (d *Delivery) resolveSealedEnv(ctx context.Context, state *agentv1.DesiredNodeState) error {
	secrets := d.store.secrets
	if secrets == nil || state == nil {
		return nil
	}
	services := state.GetServices()
	if len(services) == 0 {
		return nil
	}
	serviceIDs := make([]string, 0, len(services))
	deploymentIDs := make([]string, 0, len(services))
	seenServices := map[string]bool{}
	seenDeployments := map[string]bool{}
	for _, svc := range services {
		if svc.GetServiceId() != "" && !seenServices[svc.GetServiceId()] {
			seenServices[svc.GetServiceId()] = true
			serviceIDs = append(serviceIDs, svc.GetServiceId())
		}
		if svc.GetDeploymentId() != "" && !seenDeployments[svc.GetDeploymentId()] {
			seenDeployments[svc.GetDeploymentId()] = true
			deploymentIDs = append(deploymentIDs, svc.GetDeploymentId())
		}
	}
	pins, err := d.deploymentSealedPins(ctx, deploymentIDs)
	if err != nil {
		return err
	}
	resolved, err := secrets.Sealed().ResolveMany(ctx, d.store.db, serviceIDs, pins)
	if err != nil {
		return err
	}
	for _, svc := range services {
		values := resolved[svc.GetServiceId()]
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

// deploymentSealedPins loads variable_versions_json for assignments'
// deployments and returns the sealed-relevant pins per service. Public keys
// are ignored here: only names with sealed versions participate. A
// deployment that predates a sealed name simply has no pin for it, and the
// name resolves to current.
func (d *Delivery) deploymentSealedPins(ctx context.Context, deploymentIDs []string) (map[string]map[string]int64, error) {
	out := map[string]map[string]int64{}
	if len(deploymentIDs) == 0 {
		return out, nil
	}
	rows, err := d.store.db.QueryContext(ctx,
		`SELECT service_id, variable_versions_json FROM deployments WHERE id = ANY($1)`, deploymentIDs)
	if err != nil {
		return nil, fmt.Errorf("load deployment variable versions: %w", err)
	}
	defer rows.Close()
	type deploymentPin struct {
		serviceID string
		versions  map[string]int64
	}
	var pins []deploymentPin
	services := map[string]bool{}
	for rows.Next() {
		var serviceID string
		var raw []byte
		if err := rows.Scan(&serviceID, &raw); err != nil {
			return nil, fmt.Errorf("scan deployment variable versions: %w", err)
		}
		var versions map[string]int64
		if err := json.Unmarshal(raw, &versions); err != nil {
			return nil, fmt.Errorf("decode deployment variable versions: %w", err)
		}
		if len(versions) == 0 {
			continue
		}
		pins = append(pins, deploymentPin{serviceID: serviceID, versions: versions})
		services[serviceID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load deployment variable versions: %w", err)
	}
	// A pinned version is honored only when that exact sealed version
	// exists. variable_versions also carries public keys mapped to spec
	// revisions, and a name may have been public at deploy time and sealed
	// later: without the existence check a stale spec revision would pin a
	// nonexistent sealed version and wedge delivery.
	history := map[string]map[string]map[int64]bool{}
	for serviceID := range services {
		known, err := sealedVersionSet(ctx, d.store.db, serviceID)
		if err != nil {
			return nil, err
		}
		history[serviceID] = known
	}
	for _, pin := range pins {
		for name, version := range pin.versions {
			if version <= 0 {
				continue
			}
			if !history[pin.serviceID][name][version] {
				continue
			}
			if out[pin.serviceID] == nil {
				out[pin.serviceID] = map[string]int64{}
			}
			out[pin.serviceID][name] = version
		}
	}
	return out, nil
}

// sealedVersionSet returns the existing (name, version) pairs for a service,
// including tombstoned names: pinned deployment reads resolve captured
// versions even after deletion.
func sealedVersionSet(ctx context.Context, q secretkeys.Querier, serviceID string) (map[string]map[int64]bool, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT name, version FROM service_secret_versions WHERE service_id = $1`, serviceID)
	if err != nil {
		return nil, fmt.Errorf("load sealed secret history: %w", err)
	}
	defer rows.Close()
	out := map[string]map[int64]bool{}
	for rows.Next() {
		var name string
		var version int64
		if err := rows.Scan(&name, &version); err != nil {
			return nil, fmt.Errorf("scan sealed secret history: %w", err)
		}
		if out[name] == nil {
			out[name] = map[int64]bool{}
		}
		out[name][version] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load sealed secret history: %w", err)
	}
	return out, nil
}
