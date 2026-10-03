package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/journal"
	"ebof-wg-mesh/internal/controlplane/secretkeys"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Runtime env is part of a service spec everywhere except storage: each
// immutable spec revision keeps its env map encrypted under the environment's
// DEK, bound to (service, revision), and spec_json carries everything else.
// Authorized reads and agent desired state are the only paths that decrypt.

var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

const (
	MaxEnvNameLength = 128
	MaxEnvValueBytes = 64 * 1024
	// ReservedEnvPrefix names the variables the platform injects into every workload.
	ReservedEnvPrefix = "PLATFORM_"
)

var errEnvEncryptionUnavailable = errors.New("service environment encryption is not configured")

// ValidateServiceEnv rejects variable names that are not identifiers, use the
// reserved PLATFORM_ prefix, or exceed the caps. Errors never echo values.
func ValidateServiceEnv(spec *platformv1.ServiceSpec) error {
	env := spec.GetRuntime().GetEnv()
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if len(name) > MaxEnvNameLength || !envNamePattern.MatchString(name) {
			return fmt.Errorf("%w: %q must match [A-Za-z_][A-Za-z0-9_]* and be 1-%d characters",
				ErrInvalidServiceEnv, name, MaxEnvNameLength)
		}
		if strings.HasPrefix(name, ReservedEnvPrefix) {
			return fmt.Errorf("%w: %q uses the reserved %s prefix", ErrInvalidServiceEnv, name, ReservedEnvPrefix)
		}
		if len(env[name]) > MaxEnvValueBytes {
			return fmt.Errorf("%w: %q exceeds the %d KiB value limit", ErrInvalidServiceEnv, name, MaxEnvValueBytes/1024)
		}
	}
	return nil
}

// revisionEnvAAD binds env ciphertext to its revision so a copied row does not decrypt.
func revisionEnvAAD(serviceID string, specRevision int64) []byte {
	return []byte("service-env/v1\x00" + serviceID + "\x00" + strconv.FormatInt(specRevision, 10))
}

// insertServiceRevisionTx persists an immutable spec revision with its env
// encrypted apart from spec_json, so plaintext never reaches the database or journal.
func (s *persistence) insertServiceRevisionTx(ctx context.Context, q ServiceQueryer, environmentID, serviceID string, specRevision int64, spec *platformv1.ServiceSpec, now time.Time) error {
	stored := spec
	var envDEKID, envCiphertext any
	if env := spec.GetRuntime().GetEnv(); len(env) > 0 {
		if s.secrets == nil {
			return errEnvEncryptionUnavailable
		}
		plaintext, err := json.Marshal(env)
		if err != nil {
			return err
		}
		encrypted, err := s.secrets.Encrypt(ctx, q, environmentID, revisionEnvAAD(serviceID, specRevision), plaintext)
		clear(plaintext)
		if err != nil {
			return fmt.Errorf("encrypt service %s env: %w", serviceID, err)
		}
		envDEKID, envCiphertext = encrypted.DEKID, encrypted.Data
		stored = proto.Clone(spec).(*platformv1.ServiceSpec)
		stored.Runtime.Env = nil
	}
	specJSON, err := protojson.Marshal(stored)
	if err != nil {
		return err
	}
	_, err = journal.RevisionRow(serviceID, specRevision).Exec(ctx, q,
		`INSERT INTO service_revisions(service_id, spec_revision, spec_json, env_dek_id, env_ciphertext, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		serviceID, specRevision, specJSON, envDEKID, envCiphertext, now)
	return err
}

// decryptRevisionEnv opens one revision's env ciphertext; an empty DEK ID means no env.
func (s *persistence) decryptRevisionEnv(ctx context.Context, q ServiceQueryer, serviceID string, specRevision int64, envDEKID string, envCiphertext []byte) (map[string]string, error) {
	if envDEKID == "" {
		return nil, nil
	}
	if s.secrets == nil {
		return nil, errEnvEncryptionUnavailable
	}
	plaintext, err := s.secrets.Decrypt(ctx, q, secretkeys.Ciphertext{DEKID: envDEKID, Data: envCiphertext}, revisionEnvAAD(serviceID, specRevision))
	if err != nil {
		return nil, fmt.Errorf("decrypt service %s revision %d env: %w", serviceID, specRevision, err)
	}
	defer clear(plaintext)
	var env map[string]string
	if err := json.Unmarshal(plaintext, &env); err != nil {
		return nil, fmt.Errorf("decode service %s revision %d env", serviceID, specRevision)
	}
	return env, nil
}

// applyServiceEnv decrypts each allocation's revision env into an agent's desired
// state, only for services assigned to that agent. The revision ciphertext comes
// from the same immutable product prefix as the spec. Platform-injected
// variables win over user variables of the same name.
func (d *Delivery) applyServiceEnv(ctx context.Context, product *journal.Projection, state *agentv1.DesiredNodeState) error {
	decrypted := map[string]map[string]string{}
	for _, svc := range state.GetServices() {
		key := fmt.Sprintf("%s/%d", svc.GetServiceId(), svc.GetDesiredSpecRevision())
		revision, exists := product.Revisions[key]
		if !exists {
			return fmt.Errorf("allocation %s has no revision %s in the captured product prefix", svc.GetAllocationId(), key)
		}
		env, known := decrypted[key]
		if !known {
			var err error
			env, err = d.store.decryptRevisionEnv(ctx, d.store.db, revision.ServiceID, revision.SpecRevision, revision.EnvDEKID, revision.EnvCiphertext)
			if err != nil {
				return err
			}
			decrypted[key] = env
		}
		if len(env) == 0 || svc.GetSpec().GetRuntime() == nil {
			continue
		}
		runtime := svc.GetSpec().GetRuntime()
		merged := make(map[string]string, len(env)+len(runtime.GetEnv()))
		for name, value := range env {
			merged[name] = value
		}
		for name, value := range runtime.GetEnv() {
			merged[name] = value
		}
		runtime.Env = merged
	}
	return nil
}
