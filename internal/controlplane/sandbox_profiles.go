package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"

	"google.golang.org/protobuf/proto"
)

const productionSandboxProfileName = "production"

func productionSandboxProfile() *platformv1.SandboxProfile {
	return &platformv1.SandboxProfile{Name: productionSandboxProfileName}
}

func sandboxProfilesFromConfig(cfg config.SandboxConfig) map[string]*platformv1.SandboxProfile {
	profiles := map[string]*platformv1.SandboxProfile{
		productionSandboxProfileName: productionSandboxProfile(),
	}
	for _, configured := range cfg.CompatibilityProfiles {
		profile := &platformv1.SandboxProfile{
			Name: strings.TrimSpace(configured.Name),
			Risk: strings.TrimSpace(configured.Risk),
		}
		for _, raw := range configured.Relaxations {
			switch strings.TrimSpace(strings.ToLower(raw)) {
			case "run-as-root":
				profile.Relaxations = append(profile.Relaxations, platformv1.SandboxRelaxation_SANDBOX_RELAXATION_RUN_AS_ROOT)
			case "writable-rootfs":
				profile.Relaxations = append(profile.Relaxations, platformv1.SandboxRelaxation_SANDBOX_RELAXATION_WRITABLE_ROOT_FILESYSTEM)
			}
		}
		sort.Slice(profile.Relaxations, func(i, j int) bool { return profile.Relaxations[i] < profile.Relaxations[j] })
		profiles[profile.Name] = profile
	}
	return profiles
}

func listedSandboxProfiles(profiles map[string]*platformv1.SandboxProfile) []*platformv1.SandboxProfile {
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		if name == productionSandboxProfileName {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]*platformv1.SandboxProfile, 0, 1+len(names))
	if profile := profiles[productionSandboxProfileName]; profile != nil {
		out = append(out, proto.Clone(profile).(*platformv1.SandboxProfile))
	} else {
		out = append(out, productionSandboxProfile())
	}
	for _, name := range names {
		out = append(out, proto.Clone(profiles[name]).(*platformv1.SandboxProfile))
	}
	return out
}

type sandboxProfileAuditRecord struct {
	ActorUserID         string
	Action              string
	PreviousProfileName string
	Profile             *platformv1.SandboxProfile
	SpecRevision        int64
	CreatedAt           time.Time
}

func (s *Store) listSandboxProfileAudit(ctx context.Context, serviceID string) ([]sandboxProfileAuditRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT actor_user_id, action, previous_profile_name, profile_name, risk, relaxations, spec_revision, created_at
		  FROM sandbox_profile_audit_events
		 WHERE service_id = $1
		 ORDER BY created_at DESC, id DESC
		 LIMIT 20`, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []sandboxProfileAuditRecord
	for rows.Next() {
		var event sandboxProfileAuditRecord
		var relaxations []byte
		profile := &platformv1.SandboxProfile{}
		if err := rows.Scan(
			&event.ActorUserID, &event.Action, &event.PreviousProfileName,
			&profile.Name, &profile.Risk, &relaxations, &event.SpecRevision, &event.CreatedAt,
		); err != nil {
			return nil, err
		}
		if len(relaxations) > 0 && string(relaxations) != "null" {
			if err := json.Unmarshal(relaxations, &profile.Relaxations); err != nil {
				return nil, err
			}
		}
		event.Profile = profile
		events = append(events, event)
	}
	return events, rows.Err()
}

func toProtoSandboxProfileAudit(events []sandboxProfileAuditRecord) []*platformv1.SandboxProfileAuditEvent {
	out := make([]*platformv1.SandboxProfileAuditEvent, 0, len(events))
	for _, event := range events {
		out = append(out, &platformv1.SandboxProfileAuditEvent{
			ActorUserId:         event.ActorUserID,
			Action:              event.Action,
			PreviousProfileName: event.PreviousProfileName,
			Profile:             event.Profile,
			SpecRevision:        event.SpecRevision,
			CreatedAt:           ts(event.CreatedAt),
		})
	}
	return out
}

func resolveServiceSandboxProfile(spec *platformv1.ServiceSpec, profiles map[string]*platformv1.SandboxProfile) error {
	if spec == nil || spec.GetRuntime() == nil {
		return nil
	}
	name := strings.TrimSpace(spec.GetRuntime().GetSandboxProfile().GetName())
	if name == "" {
		name = productionSandboxProfileName
	}
	profile, ok := profiles[name]
	if !ok {
		return fmt.Errorf("sandbox profile %q is not enabled by the operator", name)
	}
	spec.Runtime.SandboxProfile = proto.Clone(profile).(*platformv1.SandboxProfile)
	return nil
}

func sandboxProfileName(spec *platformv1.ServiceSpec) string {
	name := strings.TrimSpace(spec.GetRuntime().GetSandboxProfile().GetName())
	if name == "" {
		return productionSandboxProfileName
	}
	return name
}

func sandboxProfileIsRelaxed(spec *platformv1.ServiceSpec) bool {
	profile := spec.GetRuntime().GetSandboxProfile()
	return profile != nil && len(profile.GetRelaxations()) > 0
}

func (s *Store) insertSandboxProfileAuditTx(
	ctx context.Context,
	tx *sql.Tx,
	serviceID, actorUserID, action, previousProfileName string,
	profile *platformv1.SandboxProfile,
	specRevision int64,
	now time.Time,
) error {
	if profile == nil {
		profile = productionSandboxProfile()
	}
	relaxations, err := json.Marshal(profile.GetRelaxations())
	if err != nil {
		return err
	}
	if strings.TrimSpace(actorUserID) == "" {
		actorUserID = "system"
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO sandbox_profile_audit_events(
			id, service_id, actor_user_id, action, previous_profile_name,
			profile_name, risk, relaxations, spec_revision, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		mustID(), serviceID, actorUserID, action, previousProfileName,
		profile.GetName(), profile.GetRisk(), relaxations, specRevision, now,
	)
	return err
}
