package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/authz"
	"ebof-wg-mesh/internal/controlplane/registry"
)

// preResolveDirectImage pins a creation spec's direct image before the
// product transaction. Empty when the spec carries no direct image.
func (d *Delivery) preResolveDirectImage(ctx context.Context, spec *platformv1.ServiceSpec) (resolvedDirectImage, error) {
	input := strings.TrimSpace(directImageRef(spec))
	if input == "" {
		return resolvedDirectImage{}, nil
	}
	pinned, err := d.resolveOneDirectImage(ctx, input)
	if err != nil {
		return resolvedDirectImage{}, err
	}
	return resolvedDirectImage{input: input, resolved: pinned}, nil
}

// errDirectImageChanged aborts a release whose spec raced tag resolution.
// The caller retries with a fresh pre-read instead of deploying a stale tag.
var errDirectImageChanged = errors.New("direct image changed during release")

// resolvedDirectImage pairs a pre-read direct-image input with the pinned
// resolution the release deploys.
type resolvedDirectImage struct {
	input    string
	resolved registry.ResolvedImage
}

// directImageInputs lists the current direct-image refs of live services
// in the environment without taking locks. The release transaction
// re-verifies each input before using its pre-resolved digest.
func (s *persistence) directImageInputs(ctx context.Context, env authz.Environment) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.id, r.spec_json
		   FROM services s
		   JOIN service_revisions r ON r.service_id = s.id AND r.spec_revision = s.current_spec_revision
		  WHERE s.environment_id = $1 AND s.deleted_at IS NULL`,
		env.ID(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		spec, err := LoadServiceSpec(raw)
		if err != nil {
			return nil, err
		}
		if image := strings.TrimSpace(directImageRef(spec)); image != "" {
			out[id] = image
		}
	}
	return out, rows.Err()
}

// resolveDirectImages pins every direct-image input outside the product
// transaction. One bad tag fails the release before anything is written.
func (d *Delivery) resolveDirectImages(ctx context.Context, inputs map[string]string) (map[string]resolvedDirectImage, error) {
	resolved := make(map[string]resolvedDirectImage, len(inputs))
	for serviceID, input := range inputs {
		pinned, err := d.resolveOneDirectImage(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("service %s: %w", serviceID, err)
		}
		resolved[serviceID] = resolvedDirectImage{input: input, resolved: pinned}
	}
	return resolved, nil
}

func (d *Delivery) resolveOneDirectImage(ctx context.Context, input string) (registry.ResolvedImage, error) {
	if d.imageResolver != nil {
		return d.imageResolver.Resolve(ctx, input)
	}
	parsed, err := registry.ParseReference(input)
	if err != nil {
		return registry.ResolvedImage{}, err
	}
	if !parsed.Pinned() {
		return registry.ResolvedImage{}, fmt.Errorf("image %q is a mutable tag and no image resolver is configured; use a digest-pinned reference", strings.TrimSpace(input))
	}
	return registry.ResolvedImage{Repository: parsed.Repository, ManifestDigest: parsed.Digest, Ref: parsed.PinnedRef()}, nil
}

// directImageArtifactTx records the pre-resolved digest as this service's
// artifact, reusing the row when an earlier release already pinned the
// same digest. The spec input must still match the pre-read: a concurrent
// update retries the release rather than deploying a stale tag.
func (d *Delivery) directImageArtifactTx(ctx context.Context, tx *sql.Tx, serviceID, input string, pre resolvedDirectImage, actor deploymentActor, now time.Time) (BuildArtifactRecord, error) {
	if strings.TrimSpace(input) == "" {
		return BuildArtifactRecord{}, errors.New("direct image is required")
	}
	if pre.input != strings.TrimSpace(input) || pre.resolved.Ref == "" {
		return BuildArtifactRecord{}, errDirectImageChanged
	}
	return d.store.insertBuildArtifactTx(ctx, tx, insertArtifactParams{
		ServiceID:           serviceID,
		Kind:                BuildArtifactDirectImage,
		ImageRepository:     pre.resolved.Repository,
		ImageManifestDigest: pre.resolved.ManifestDigest,
		SourceImageRef:      strings.TrimSpace(input),
		BuildActorKind:      actor.Kind,
		BuildActorID:        actor.ID,
		CreatedAt:           now,
	})
}
