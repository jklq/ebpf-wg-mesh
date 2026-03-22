package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

var (
	errVolumeInUse          = errors.New("volume still referenced by service")
	errVolumeNotFound       = errors.New("volume not found")
	errVolumeAgentMismatch  = errors.New("volume bound to different agent")
	errConcurrentUpdate     = errors.New("concurrent service update")
	errDomainAlreadyExists  = errors.New("domain binding already exists")
	errNoPlacementAvailable = errors.New("no healthy agent satisfies placement")
)

func volumeKey(projectID, name string) string {
	var b strings.Builder
	b.Grow(len(projectID) + 1 + len(name))
	b.WriteString(projectID)
	b.WriteByte(0)
	b.WriteString(name)
	return b.String()
}

type serviceQueryer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func canonicalServiceSpec(spec *platformv1.ServiceSpec) *platformv1.ServiceSpec {
	if spec == nil {
		return nil
	}
	out := proto.Clone(spec).(*platformv1.ServiceSpec)
	runtime := out.GetRuntime()
	if runtime != nil {
		if len(runtime.Command) == 0 {
			runtime.Command = nil
		}
		if len(runtime.Args) == 0 {
			runtime.Args = nil
		}
		if len(runtime.Env) == 0 {
			runtime.Env = nil
		}
		if hc := runtime.GetHealthCheck(); hc != nil &&
			hc.GetType() == platformv1.HealthCheck_TYPE_UNSPECIFIED &&
			hc.GetPath() == "" &&
			hc.GetPort() == 0 &&
			hc.GetIntervalSeconds() == 0 &&
			hc.GetTimeoutSeconds() == 0 {
			runtime.HealthCheck = nil
		}
	}
	if source := out.GetSource(); source != nil {
		if spec := source.GetSourceSpec(); spec != nil {
			spec.Provider = strings.TrimSpace(strings.ToLower(spec.GetProvider()))
			spec.RepositorySelector = strings.TrimSpace(strings.ToLower(spec.GetRepositorySelector()))
			spec.TrackedRef = strings.TrimSpace(spec.GetTrackedRef())
			if spec.TrackedRef == "" {
				spec.TrackedRef = "main"
			}
			if spec.BuildRecipe == nil {
				spec.BuildRecipe = &platformv1.BuildRecipe{}
			}
			if spec.BuildRecipe.DockerfilePath == "" {
				spec.BuildRecipe.DockerfilePath = "Dockerfile"
			}
			if spec.BuildRecipe.ContextDir == "" {
				spec.BuildRecipe.ContextDir = "."
			}
		}
	}
	return out
}

func serviceRuntime(spec *platformv1.ServiceSpec) *platformv1.ServiceRuntime {
	if spec == nil {
		return nil
	}
	return spec.GetRuntime()
}

func serviceVolumeName(spec *platformv1.ServiceSpec) string {
	return serviceRuntime(spec).GetVolumeName()
}

func directImageRef(spec *platformv1.ServiceSpec) string {
	if spec == nil || spec.GetSource() == nil {
		return ""
	}
	return spec.GetSource().GetImage().GetImage()
}

func desiredSourceSpec(spec *platformv1.ServiceSpec) *platformv1.ServiceSourceSpec {
	if spec == nil || spec.GetSource() == nil {
		return nil
	}
	return spec.GetSource().GetSourceSpec()
}

func sameDesiredSourceSpec(a, b *platformv1.ServiceSpec) bool {
	as := desiredSourceSpec(a)
	bs := desiredSourceSpec(b)
	if (as == nil) != (bs == nil) {
		return false
	}
	if as == nil {
		return true
	}
	if strings.TrimSpace(strings.ToLower(as.GetProvider())) != strings.TrimSpace(strings.ToLower(bs.GetProvider())) {
		return false
	}
	if strings.TrimSpace(strings.ToLower(as.GetRepositorySelector())) != strings.TrimSpace(strings.ToLower(bs.GetRepositorySelector())) {
		return false
	}
	at := as.GetTrackedRef()
	bt := bs.GetTrackedRef()
	if at != bt && !(at == "" && bt == "main") && !(at == "main" && bt == "") {
		return false
	}
	ab := as.GetBuildRecipe()
	bb := bs.GetBuildRecipe()
	if (ab == nil) != (bb == nil) {
		return false
	}
	if ab == nil {
		return true
	}
	adp := ab.GetDockerfilePath()
	bdp := bb.GetDockerfilePath()
	if adp != bdp && !(adp == "" && bdp == "Dockerfile") && !(adp == "Dockerfile" && bdp == "") {
		return false
	}
	acd := ab.GetContextDir()
	bcd := bb.GetContextDir()
	if acd != bcd && !(acd == "" && bcd == ".") && !(acd == "." && bcd == "") {
		return false
	}
	return true
}

func resolvedServiceSpec(spec *platformv1.ServiceSpec, resolvedImage string) *platformv1.ResolvedServiceSpec {
	if spec == nil {
		return nil
	}
	image := resolvedImage
	if image == "" {
		image = directImageRef(spec)
	}
	var runtime *platformv1.ServiceRuntime
	if spec.GetRuntime() != nil {
		runtime = proto.Clone(spec.GetRuntime()).(*platformv1.ServiceRuntime)
	}
	return &platformv1.ResolvedServiceSpec{
		Image:   image,
		Runtime: runtime,
	}
}

func buildSourceSummary(spec *platformv1.ServiceSpec) *platformv1.ServiceSourceSummary {
	if spec == nil || spec.GetSource() == nil {
		return nil
	}
	switch src := spec.GetSource().Source.(type) {
	case *platformv1.ServiceSource_Image:
		return &platformv1.ServiceSourceSummary{
			Source: &platformv1.ServiceSourceSummary_Image{
				Image: proto.Clone(src.Image).(*platformv1.DirectImageSource),
			},
		}
	}
	return nil
}

type placementCandidate struct {
	ID                     string
	CPUMillisCapacity      int64
	MemoryMebibytesCapcity int64
	ServiceCount           int64
	UsedCPUMillis          int64
	UsedMemoryMebibytes    int64
}

func sortedDomains(domains []string) []string {
	out := append([]string(nil), domains...)
	sort.Strings(out)
	return out
}

func sameServiceSpec(a, b *platformv1.ServiceSpec) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if !proto.Equal(a, b) {
		return false
	}
	return equalServiceSpecAfterCanonicalization(a, b)
}

func equalServiceSpecAfterCanonicalization(a, b *platformv1.ServiceSpec) bool {
	ar := a.GetRuntime()
	br := b.GetRuntime()
	if (ar == nil) != (br == nil) {
		return false
	}
	if ar != nil {
		if !equalRuntimeAfterCanonicalization(ar, br) {
			return false
		}
	}
	as := a.GetSource()
	bs := b.GetSource()
	if (as == nil) != (bs == nil) {
		return false
	}
	if as != nil && bs != nil {
		ag := as.GetSourceSpec()
		bg := bs.GetSourceSpec()
		if (ag == nil) != (bg == nil) {
			return false
		}
		if ag != nil && bg != nil {
			if strings.TrimSpace(strings.ToLower(ag.GetProvider())) != strings.TrimSpace(strings.ToLower(bg.GetProvider())) {
				return false
			}
			if strings.TrimSpace(strings.ToLower(ag.GetRepositorySelector())) != strings.TrimSpace(strings.ToLower(bg.GetRepositorySelector())) {
				return false
			}
			ab := ag.GetTrackedRef()
			bb := bg.GetTrackedRef()
			if ab != bb && !(ab == "" && bb == "main") && !(ab == "main" && bb == "") {
				return false
			}
			adp := ag.GetBuildRecipe().GetDockerfilePath()
			bdp := bg.GetBuildRecipe().GetDockerfilePath()
			if adp != bdp && !(adp == "" && bdp == "Dockerfile") && !(adp == "Dockerfile" && bdp == "") {
				return false
			}
			acd := ag.GetBuildRecipe().GetContextDir()
			bcd := bg.GetBuildRecipe().GetContextDir()
			if acd != bcd && !(acd == "" && bcd == ".") && !(acd == "." && bcd == "") {
				return false
			}
		}
	}
	return true
}

func equalRuntimeAfterCanonicalization(a, b *platformv1.ServiceRuntime) bool {
	ac := a.GetCommand()
	bc := b.GetCommand()
	switch {
	case len(ac) == 0 && len(bc) == 0:
	case len(ac) == 0 && len(bc) > 0:
		return false
	case len(ac) > 0 && len(bc) == 0:
		return false
	case len(ac) > 0 && len(bc) > 0:
		if len(ac) != len(bc) {
			return false
		}
		for i := range ac {
			if ac[i] != bc[i] {
				return false
			}
		}
	default:
		return false
	}

	aa := a.GetArgs()
	ba := b.GetArgs()
	switch {
	case len(aa) == 0 && len(ba) == 0:
	case len(aa) == 0 && len(ba) > 0:
		return false
	case len(aa) > 0 && len(ba) == 0:
		return false
	case len(aa) > 0 && len(ba) > 0:
		if len(aa) != len(ba) {
			return false
		}
		for i := range aa {
			if aa[i] != ba[i] {
				return false
			}
		}
	default:
		return false
	}

	ae := a.GetEnv()
	be := b.GetEnv()
	switch {
	case len(ae) == 0 && len(be) == 0:
	case len(ae) == 0 && len(be) > 0:
		return false
	case len(ae) > 0 && len(be) == 0:
		return false
	case len(ae) > 0 && len(be) > 0:
		if len(ae) != len(be) {
			return false
		}
		for i := range ae {
			if ae[i] != be[i] {
				return false
			}
		}
	default:
		return false
	}

	ahc := a.GetHealthCheck()
	bhc := b.GetHealthCheck()
	if (ahc == nil) != (bhc == nil) {
		return false
	}
	if ahc != nil {
		ahct := ahc.GetType()
		bhct := bhc.GetType()
		normalizedAHC := ahct == platformv1.HealthCheck_TYPE_UNSPECIFIED
		normalizedBHC := bhct == platformv1.HealthCheck_TYPE_UNSPECIFIED
		ahcp := ahc.GetPath()
		bhcp := bhc.GetPath()
		ahcpo := ahc.GetPort()
		bhcpo := bhc.GetPort()
		ahcis := ahc.GetIntervalSeconds()
		bhcis := bhc.GetIntervalSeconds()
		ahcts := ahc.GetTimeoutSeconds()
		bhcts := bhc.GetTimeoutSeconds()
		if normalizedAHC && ahcp == "" && ahcpo == 0 && ahcis == 0 && ahcts == 0 {
			normalizedAHC = true
		} else {
			normalizedAHC = false
		}
		if normalizedBHC && bhcp == "" && bhcpo == 0 && bhcis == 0 && bhcts == 0 {
			normalizedBHC = true
		} else {
			normalizedBHC = false
		}
		if normalizedAHC != normalizedBHC {
			return false
		}
	}
	return true
}

func loadServiceSpec(raw []byte) (*platformv1.ServiceSpec, error) {
	spec := &platformv1.ServiceSpec{}
	if err := protojson.Unmarshal(raw, spec); err != nil {
		return nil, err
	}
	return canonicalServiceSpec(spec), nil
}

func (s *Store) createVolumeTx(ctx context.Context, tx *sql.Tx, subject, projectID, name string, sizeBytes int64, agentID string) (volumeRecord, error) {
	if _, err := s.projectByIDQuerier(ctx, tx, subject, projectID); err != nil {
		return volumeRecord{}, err
	}
	rec := volumeRecord{
		ID:           mustID(),
		ProjectID:    projectID,
		Name:         name,
		SizeBytes:    sizeBytes,
		BoundAgentID: agentID,
		CreatedAt:    time.Now().UTC(),
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO volumes(id, project_id, name, size_bytes, bound_agent_id, created_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		rec.ID, rec.ProjectID, rec.Name, rec.SizeBytes, rec.BoundAgentID, rec.CreatedAt,
	); err != nil {
		return volumeRecord{}, err
	}
	if err := s.bumpDesiredRevisionsTx(ctx, tx, []string{rec.BoundAgentID}); err != nil {
		return volumeRecord{}, err
	}
	return rec, nil
}

func (s *Store) createServiceTx(ctx context.Context, tx *sql.Tx, subject, projectID, name string, spec *platformv1.ServiceSpec, agentID string) (serviceRecord, error) {
	if _, err := s.projectByIDQuerier(ctx, tx, subject, projectID); err != nil {
		return serviceRecord{}, err
	}
	return s.createServiceTxInternal(ctx, tx, projectID, name, spec, agentID)
}

func (s *Store) createServiceTxInternal(ctx context.Context, tx *sql.Tx, projectID, name string, spec *platformv1.ServiceSpec, agentID string) (serviceRecord, error) {
	if _, err := s.projectByIDInternalQuerier(ctx, tx, projectID); err != nil {
		return serviceRecord{}, err
	}
	if agentID == "" {
		return serviceRecord{}, errors.New("agent id required")
	}
	if volumeName := serviceVolumeName(spec); volumeName != "" {
		volumeAgentID, err := s.boundAgentForVolumeQuerier(ctx, tx, projectID, volumeName)
		if err != nil {
			return serviceRecord{}, err
		}
		if volumeAgentID != agentID {
			return serviceRecord{}, fmt.Errorf("%w: volume %q is bound to %s, service is scheduled to %s", errVolumeAgentMismatch, volumeName, volumeAgentID, agentID)
		}
	}

	now := time.Now().UTC()
	spec = canonicalServiceSpec(spec)
	rec := serviceRecord{
		ID:                mustID(),
		ProjectID:         projectID,
		Name:              name,
		Spec:              spec,
		SpecRevision:      1,
		RolloutGeneration: 1,
		AllocatedAgentID:  agentID,
		ResolvedImage:     directImageRef(spec),
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if source := desiredSourceSpec(spec); source != nil {
		rec.SourceSummary = toProtoSourceStateSummary(source, nil, nil, nil)
	} else {
		rec.SourceSummary = buildSourceSummary(spec)
	}
	specJSON, err := protojson.Marshal(spec)
	if err != nil {
		return serviceRecord{}, err
	}
	allocationID := mustID()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO services(
			id, project_id, name, current_spec_revision, current_rollout_generation, allocated_agent_id,
			current_resolved_image, last_successful_commit_sha, latest_build_id, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		rec.ID, rec.ProjectID, rec.Name, rec.SpecRevision, rec.RolloutGeneration, rec.AllocatedAgentID, rec.ResolvedImage, rec.LastSuccessfulCommitSHA, rec.LatestBuildID, now, now,
	); err != nil {
		return serviceRecord{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO service_revisions(service_id, spec_revision, spec_json, created_at) VALUES ($1, $2, $3, $4)`,
		rec.ID, rec.SpecRevision, specJSON, now,
	); err != nil {
		return serviceRecord{}, err
	}
	if err := s.insertServiceRolloutTx(ctx, tx, rec.ID, rec.RolloutGeneration, rec.SpecRevision, "create", "", "", now); err != nil {
		return serviceRecord{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO allocations(
			id, service_id, project_id, agent_id,
			desired_spec_revision, applied_spec_revision,
			desired_rollout_generation, applied_rollout_generation,
			phase, message, endpoint_addr, healthy, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		allocationID, rec.ID, rec.ProjectID, rec.AllocatedAgentID, rec.SpecRevision, 0, rec.RolloutGeneration, 0, "Pending", "", "", false, now,
	); err != nil {
		return serviceRecord{}, err
	}
	if desiredSourceSpec(spec) != nil {
		if err := s.enqueueSourceSpecChangedTx(ctx, tx, rec.ID, rec.SpecRevision, false); err != nil {
			return serviceRecord{}, err
		}
	}
	if err := s.bumpDesiredRevisionsTx(ctx, tx, []string{rec.AllocatedAgentID}); err != nil {
		return serviceRecord{}, err
	}
	return rec, nil
}

func (s *Store) ensureManagedService(ctx context.Context, projectID, name string, spec *platformv1.ServiceSpec) (serviceRecord, error) {
	var rec serviceRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		current, found, err := s.serviceByNameQuerier(ctx, tx, projectID, name)
		if err != nil {
			return err
		}
		if !found {
			agentID, err := s.chooseAgentForServiceTx(ctx, tx, projectID, spec)
			if err != nil {
				return err
			}
			rec, err = s.createServiceTxInternal(ctx, tx, projectID, name, spec, agentID)
			return err
		}
		spec = canonicalServiceSpec(spec)
		if sameServiceSpec(current.Spec, spec) {
			rec = current
			return nil
		}
		now := time.Now().UTC()
		nextSpecRevision := current.SpecRevision + 1
		nextRolloutGeneration := current.RolloutGeneration + 1
		specJSON, err := protojson.Marshal(spec)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE services
			    SET current_spec_revision = $1,
			        current_rollout_generation = $2,
			        updated_at = $3
			  WHERE id = $4`,
			nextSpecRevision,
			nextRolloutGeneration,
			now,
			current.ID,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO service_revisions(service_id, spec_revision, spec_json, created_at) VALUES ($1, $2, $3, $4)`,
			current.ID,
			nextSpecRevision,
			specJSON,
			now,
		); err != nil {
			return err
		}
		if err := s.insertServiceRolloutTx(ctx, tx, current.ID, nextRolloutGeneration, nextSpecRevision, "managed-sync", "", "", now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE allocations
			    SET desired_spec_revision = $1,
			        desired_rollout_generation = $2,
			        phase = $3,
			        message = $4,
			        healthy = $5,
			        updated_at = $6
			  WHERE service_id = $7`,
			nextSpecRevision,
			nextRolloutGeneration,
			"Pending",
			"",
			false,
			now,
			current.ID,
		); err != nil {
			return err
		}
		rec = current
		rec.Spec = spec
		rec.SpecRevision = nextSpecRevision
		rec.RolloutGeneration = nextRolloutGeneration
		rec.UpdatedAt = now
		if desiredSourceSpec(spec) != nil {
			if err := s.enqueueSourceSpecChangedTx(ctx, tx, rec.ID, rec.SpecRevision, false); err != nil {
				return err
			}
		}
		if err := s.bumpDesiredRevisionsTx(ctx, tx, []string{current.AllocatedAgentID}); err != nil {
			return err
		}
		rec.RolloutGeneration = nextRolloutGeneration
		rec.UpdatedAt = now
		return nil
	})
	if err != nil {
		return serviceRecord{}, err
	}
	return rec, nil
}

func (s *Store) ensureManagedDomainBinding(ctx context.Context, projectID, hostname, serviceID string) (domainBindingRecord, error) {
	var binding domainBindingRecord
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.projectByIDInternalQuerier(ctx, tx, projectID); err != nil {
			return err
		}
		var existingServiceID string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM services WHERE id = $1 AND project_id = $2`, serviceID, projectID).Scan(&existingServiceID); err != nil {
			return err
		}

		now := time.Now().UTC()
		err := tx.QueryRowContext(ctx,
			`SELECT hostname, project_id, service_id, created_at, updated_at
			   FROM domain_bindings
			  WHERE hostname = $1`,
			hostname,
		).Scan(&binding.Hostname, &binding.ProjectID, &binding.ServiceID, &binding.CreatedAt, &binding.UpdatedAt)
		switch {
		case err == sql.ErrNoRows:
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO domain_bindings(hostname, project_id, service_id, created_at, updated_at)
				 VALUES ($1, $2, $3, $4, $5)`,
				hostname, projectID, serviceID, now, now,
			); err != nil {
				return err
			}
			binding = domainBindingRecord{
				Hostname:  hostname,
				ProjectID: projectID,
				ServiceID: serviceID,
				CreatedAt: now,
				UpdatedAt: now,
			}
			return nil
		case err != nil:
			return err
		case binding.ProjectID != projectID:
			return errDomainAlreadyExists
		case binding.ServiceID == serviceID:
			return nil
		default:
			if _, err := tx.ExecContext(ctx,
				`UPDATE domain_bindings
				    SET service_id = $1,
				        updated_at = $2
				  WHERE hostname = $3 AND project_id = $4`,
				serviceID, now, hostname, projectID,
			); err != nil {
				return err
			}
			binding.ServiceID = serviceID
			binding.UpdatedAt = now
			return nil
		}
	})
	if err != nil {
		return domainBindingRecord{}, err
	}
	return binding, nil
}

func (s *Store) chooseAgentForVolumeTx(ctx context.Context, tx *sql.Tx) (string, error) {
	return s.chooseAgentForPlacementQuerier(ctx, tx, nil)
}

func (s *Store) chooseAgentForServiceTx(ctx context.Context, tx *sql.Tx, projectID string, spec *platformv1.ServiceSpec) (string, error) {
	if volumeName := serviceVolumeName(spec); volumeName != "" {
		return s.boundAgentForVolumeQuerier(ctx, tx, projectID, volumeName)
	}
	return s.chooseAgentForPlacementQuerier(ctx, tx, spec)
}

func (s *Store) chooseAgentForPlacementQuerier(ctx context.Context, q serviceQueryer, spec *platformv1.ServiceSpec) (string, error) {
	candidates, err := s.placementCandidatesQuerier(ctx, q)
	if err != nil {
		return "", err
	}
	for _, candidate := range candidates {
		if spec != nil {
			runtime := serviceRuntime(spec)
			if candidate.CPUMillisCapacity > 0 && candidate.UsedCPUMillis+runtime.GetCpuMillis() > candidate.CPUMillisCapacity {
				continue
			}
			if candidate.MemoryMebibytesCapcity > 0 && candidate.UsedMemoryMebibytes+runtime.GetMemoryMebibytes() > candidate.MemoryMebibytesCapcity {
				continue
			}
		}
		return candidate.ID, nil
	}
	return "", errNoPlacementAvailable
}

func (s *Store) placementCandidatesQuerier(ctx context.Context, q serviceQueryer) ([]placementCandidate, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT a.id,
		        a.cpu_millis_capacity,
		        a.memory_mebibytes_capacity,
		        COALESCE(stats.service_count, 0),
		        COALESCE(stats.cpu_millis, 0),
		        COALESCE(stats.memory_mebibytes, 0)
		   FROM agents a
		   LEFT JOIN (
		        SELECT s.allocated_agent_id AS agent_id,
		               COUNT(*) AS service_count,
		               COALESCE(SUM(COALESCE((r.spec_json->'runtime'->>'cpuMillis')::INT8, 0)), 0) AS cpu_millis,
		               COALESCE(SUM(COALESCE((r.spec_json->'runtime'->>'memoryMebibytes')::INT8, 0)), 0) AS memory_mebibytes
		          FROM services s
		          JOIN service_revisions r
		            ON r.service_id = s.id
		           AND r.spec_revision = s.current_spec_revision
		         GROUP BY s.allocated_agent_id
		   ) AS stats
		     ON stats.agent_id = a.id
		  WHERE a.last_seen_at > $1
		  ORDER BY COALESCE(stats.service_count, 0) ASC, a.id ASC`,
		time.Now().UTC().Add(-30*time.Second),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var candidates []placementCandidate
	for rows.Next() {
		var candidate placementCandidate
		if err := rows.Scan(
			&candidate.ID,
			&candidate.CPUMillisCapacity,
			&candidate.MemoryMebibytesCapcity,
			&candidate.ServiceCount,
			&candidate.UsedCPUMillis,
			&candidate.UsedMemoryMebibytes,
		); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

func (s *Store) boundAgentForVolume(ctx context.Context, projectID, volumeName string) (string, error) {
	return s.boundAgentForVolumeQuerier(ctx, s.db, projectID, volumeName)
}

func (s *Store) boundAgentForVolumeQuerier(ctx context.Context, q serviceQueryer, projectID, volumeName string) (string, error) {
	var agentID string
	err := q.QueryRowContext(ctx, `SELECT bound_agent_id FROM volumes WHERE project_id = $1 AND name = $2`, projectID, volumeName).Scan(&agentID)
	switch {
	case err == nil:
		return agentID, nil
	case errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("%w: %q", errVolumeNotFound, volumeName)
	default:
		return "", err
	}
}

func (s *Store) listDesiredVolumes(ctx context.Context, agentID string) ([]*agentv1.DesiredVolume, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, project_id, name, size_bytes
		   FROM volumes
		  WHERE bound_agent_id = $1
		  ORDER BY created_at ASC`,
		agentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*agentv1.DesiredVolume
	for rows.Next() {
		vol := &agentv1.DesiredVolume{}
		if err := rows.Scan(&vol.VolumeId, &vol.ProjectId, &vol.Name, &vol.SizeBytes); err != nil {
			return nil, err
		}
		out = append(out, vol)
	}
	return out, rows.Err()
}

func (s *Store) listDesiredServices(ctx context.Context, agentID string) ([]*agentv1.DesiredService, error) {
	agent, err := s.agentByID(ctx, agentID)
	if err != nil {
		return nil, err
	}
	volumes, err := s.listDesiredVolumes(ctx, agentID)
	if err != nil {
		return nil, err
	}
	volumeIDs := make(map[string]string, len(volumes))
	for _, vol := range volumes {
		volumeIDs[volumeKey(vol.GetProjectId(), vol.GetName())] = vol.GetVolumeId()
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT a.id, s.id, s.project_id, s.name, s.current_spec_revision, s.current_rollout_generation, s.current_resolved_image, r.spec_json
		   FROM allocations a
		   JOIN services s ON s.id = a.service_id
		   JOIN service_revisions r ON r.service_id = s.id AND r.spec_revision = s.current_spec_revision
		  WHERE a.agent_id = $1
		    AND s.current_resolved_image <> ''
		  ORDER BY s.created_at ASC`,
		agentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*agentv1.DesiredService
	for rows.Next() {
		svc := &agentv1.DesiredService{}
		var resolvedImage string
		var rawSpec []byte
		if err := rows.Scan(&svc.AllocationId, &svc.ServiceId, &svc.ProjectId, &svc.Name, &svc.DesiredSpecRevision, &svc.DesiredRolloutGeneration, &resolvedImage, &rawSpec); err != nil {
			return nil, err
		}
		spec, err := loadServiceSpec(rawSpec)
		if err != nil {
			return nil, err
		}
		svc.Spec = resolvedServiceSpec(spec, resolvedImage)
		if volumeName := serviceVolumeName(spec); volumeName != "" {
			svc.VolumeId = volumeIDs[volumeKey(svc.ProjectId, volumeName)]
		}
		svc.PrivateIpv6, err = privateIPv6(agent.WorkloadIPv6Subnet, svc.ProjectId, svc.ServiceId)
		if err != nil {
			return nil, err
		}
		out = append(out, svc)
	}
	return out, rows.Err()
}

func (s *Store) listHealthyIngressBackends(ctx context.Context) ([]ingressBackend, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT d.hostname, a.endpoint_addr
		   FROM domain_bindings d
		   JOIN allocations a ON a.service_id = d.service_id
		  WHERE a.healthy = TRUE AND a.endpoint_addr <> ''
		  ORDER BY d.hostname ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var backends []ingressBackend
	for rows.Next() {
		var backend ingressBackend
		if err := rows.Scan(&backend.Domain, &backend.EndpointAddr); err != nil {
			return nil, err
		}
		backends = append(backends, backend)
	}
	return backends, rows.Err()
}

func (s *Store) markAllocationHealthyForTest(ctx context.Context, serviceID, endpointAddr string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE allocations SET healthy = TRUE, endpoint_addr = $1, updated_at = $2 WHERE service_id = $3`,
		endpointAddr, time.Now().UTC(), serviceID,
	)
	return err
}

func (s *Store) countServiceRevisionsForTest(ctx context.Context, serviceID string) (int, error) {
	var revisions int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM service_revisions WHERE service_id = $1`, serviceID).Scan(&revisions); err != nil {
		return 0, err
	}
	return revisions, nil
}

func (s *Store) countServiceRolloutsForTest(ctx context.Context, serviceID string) (int, error) {
	var rollouts int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM service_rollouts WHERE service_id = $1`, serviceID).Scan(&rollouts); err != nil {
		return 0, err
	}
	return rollouts, nil
}

type ingressBackend struct {
	Domain       string
	EndpointAddr string
}
