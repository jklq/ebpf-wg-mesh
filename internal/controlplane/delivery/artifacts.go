package delivery

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/authz"
	"ebof-wg-mesh/internal/controlplane/registry"
	"ebof-wg-mesh/internal/controlplane/source"

	"github.com/google/uuid"
)

// BuilderToolchainVersion is the platform toolchain in the reuse key: bumping it stops
// skip-rebuild from matching older images. Not a per-builder report.
const BuilderToolchainVersion = "v1"

// Build artifact kinds. Build artifacts record successful platform builds; direct-image
// artifacts record tags resolved at deploy time. Both pin runtime identity to a digest.
const (
	BuildArtifactBuild       = "build"
	BuildArtifactDirectImage = "direct_image"
)

// BuildArtifactRecord keeps immutable runtime identity and provenance. Retention
// changes image availability while preserving deployment history.
type BuildArtifactRecord struct {
	ID                   string
	ServiceID            string
	BuildID              string
	Kind                 string
	SourceSnapshotDigest string
	CommitSHA            string
	BuildRecipe          *platformv1.BuildRecipe
	BuilderVersion       string
	ImageRepository      string
	ImageManifestDigest  string
	ImageRef             string
	SourceImageRef       string
	ReuseKey             string
	BuildActorKind       string
	BuildActorID         string
	CreatedAt            time.Time
	ImageRetained        bool
}

type insertArtifactParams struct {
	ServiceID            string
	BuildID              string
	Kind                 string
	SourceSnapshotDigest string
	CommitSHA            string
	BuildRecipe          *platformv1.BuildRecipe
	BuilderVersion       string
	ImageRepository      string
	ImageManifestDigest  string
	SourceImageRef       string
	BuildActorKind       string
	BuildActorID         string
	CreatedAt            time.Time
}

const buildArtifactSelectColumns = `id, service_id, COALESCE(build_id, ''), kind,
	source_snapshot_digest, commit_sha, build_recipe_json, builder_version,
	image_repository, image_manifest_digest, image_ref, source_image_ref,
	reuse_key, build_actor_kind, build_actor_id, created_at, image_retained`

func scanBuildArtifactRow(scanner interface{ Scan(...any) error }) (BuildArtifactRecord, error) {
	var rec BuildArtifactRecord
	var recipeJSON []byte
	if err := scanner.Scan(
		&rec.ID,
		&rec.ServiceID,
		&rec.BuildID,
		&rec.Kind,
		&rec.SourceSnapshotDigest,
		&rec.CommitSHA,
		&recipeJSON,
		&rec.BuilderVersion,
		&rec.ImageRepository,
		&rec.ImageManifestDigest,
		&rec.ImageRef,
		&rec.SourceImageRef,
		&rec.ReuseKey,
		&rec.BuildActorKind,
		&rec.BuildActorID,
		&rec.CreatedAt,
		&rec.ImageRetained,
	); err != nil {
		return BuildArtifactRecord{}, err
	}
	recipe, err := source.UnmarshalBuildRecipe(recipeJSON)
	if err != nil {
		return BuildArtifactRecord{}, err
	}
	rec.BuildRecipe = recipe
	return rec, nil
}

// artifactReuseKey identifies build content for skip-rebuild: source snapshot, canonical
// recipe, toolchain version. Recipe defaults match equalDesiredSourceSpec.
func artifactReuseKey(snapshotDigest string, recipe *platformv1.BuildRecipe) string {
	builder := recipe.GetBuilder().String()
	dockerfile := recipe.GetDockerfilePath()
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	contextDir := recipe.GetContextDir()
	if contextDir == "" {
		contextDir = "."
	}
	canonical := strings.Join([]string{
		strings.TrimSpace(snapshotDigest),
		builder,
		strings.TrimSpace(dockerfile),
		strings.TrimSpace(contextDir),
		BuilderToolchainVersion,
	}, "\x00")
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

const buildArtifactInsertColumns = `id, service_id, build_id, kind, source_snapshot_digest, commit_sha,
	build_recipe_json, builder_version, image_repository, image_manifest_digest,
	image_ref, source_image_ref, reuse_key, build_actor_kind, build_actor_id, created_at`

// buildArtifactRecordFromParams validates artifact parameters and fills the derived
// identity fields: artifact id, pinned image ref, and reuse key.
func buildArtifactRecordFromParams(params insertArtifactParams) (BuildArtifactRecord, error) {
	if strings.TrimSpace(params.ServiceID) == "" {
		return BuildArtifactRecord{}, errors.New("artifact service is required")
	}
	if params.Kind != BuildArtifactBuild && params.Kind != BuildArtifactDirectImage {
		return BuildArtifactRecord{}, fmt.Errorf("unknown artifact kind %q", params.Kind)
	}
	if strings.TrimSpace(params.ImageRepository) == "" || strings.TrimSpace(params.ImageManifestDigest) == "" {
		return BuildArtifactRecord{}, errors.New("artifact image repository and manifest digest are required")
	}
	reuseKey := ""
	if params.Kind == BuildArtifactBuild {
		if strings.TrimSpace(params.SourceSnapshotDigest) == "" {
			return BuildArtifactRecord{}, errors.New("build artifact source snapshot digest is required")
		}
		reuseKey = artifactReuseKey(params.SourceSnapshotDigest, params.BuildRecipe)
	}
	return BuildArtifactRecord{
		ID:                   uuid.NewString(),
		ServiceID:            params.ServiceID,
		BuildID:              params.BuildID,
		Kind:                 params.Kind,
		SourceSnapshotDigest: params.SourceSnapshotDigest,
		CommitSHA:            params.CommitSHA,
		BuildRecipe:          params.BuildRecipe,
		BuilderVersion:       params.BuilderVersion,
		ImageRepository:      params.ImageRepository,
		ImageManifestDigest:  params.ImageManifestDigest,
		ImageRef:             params.ImageRepository + "@" + params.ImageManifestDigest,
		SourceImageRef:       params.SourceImageRef,
		ReuseKey:             reuseKey,
		BuildActorKind:       params.BuildActorKind,
		BuildActorID:         params.BuildActorID,
		CreatedAt:            params.CreatedAt,
		ImageRetained:        true,
	}, nil
}

func buildArtifactInsertArgs(rec BuildArtifactRecord) ([]any, error) {
	recipeJSON, err := source.MarshalBuildRecipe(rec.BuildRecipe)
	if err != nil {
		return nil, err
	}
	return []any{
		rec.ID, rec.ServiceID, rec.BuildID, rec.Kind, rec.SourceSnapshotDigest, rec.CommitSHA,
		recipeJSON, rec.BuilderVersion, rec.ImageRepository, rec.ImageManifestDigest,
		rec.ImageRef, rec.SourceImageRef, rec.ReuseKey, rec.BuildActorKind, rec.BuildActorID, rec.CreatedAt,
	}, nil
}

// insertBuildArtifactTx records the immutable artifact of one successful build. Every build
// records its own row — provenance belongs to the producing build — so equal digests keep
// distinct rows. Callers pass digest-pinned images only.
func (s *persistence) insertBuildArtifactTx(ctx context.Context, tx *sql.Tx, params insertArtifactParams) (BuildArtifactRecord, error) {
	if params.Kind != BuildArtifactBuild {
		return BuildArtifactRecord{}, fmt.Errorf("insertBuildArtifactTx requires kind %q, got %q", BuildArtifactBuild, params.Kind)
	}
	rec, err := buildArtifactRecordFromParams(params)
	if err != nil {
		return BuildArtifactRecord{}, err
	}
	args, err := buildArtifactInsertArgs(rec)
	if err != nil {
		return BuildArtifactRecord{}, err
	}
	return scanBuildArtifactRow(tx.QueryRowContext(ctx,
		`INSERT INTO build_artifacts(`+buildArtifactInsertColumns+`)
		 VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		 RETURNING `+buildArtifactSelectColumns,
		args...,
	))
}

// insertDirectImageArtifactTx records a resolved direct image, converging concurrent
// releases on one row per (service, image).
func (s *persistence) insertDirectImageArtifactTx(ctx context.Context, tx *sql.Tx, params insertArtifactParams) (BuildArtifactRecord, error) {
	if params.Kind != BuildArtifactDirectImage {
		return BuildArtifactRecord{}, fmt.Errorf("insertDirectImageArtifactTx requires kind %q, got %q", BuildArtifactDirectImage, params.Kind)
	}
	rec, err := buildArtifactRecordFromParams(params)
	if err != nil {
		return BuildArtifactRecord{}, err
	}
	var deleting string
	if err := tx.QueryRowContext(ctx, `SELECT image_ref FROM registry_image_deletions WHERE image_ref = $1 FOR UPDATE`, rec.ImageRef).Scan(&deleting); err == nil {
		return BuildArtifactRecord{}, fmt.Errorf("%w: platform image has expired; rebuild from source", registry.ErrImageNotFound)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return BuildArtifactRecord{}, err
	}
	args, err := buildArtifactInsertArgs(rec)
	if err != nil {
		return BuildArtifactRecord{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO build_artifacts(`+buildArtifactInsertColumns+`)
		 VALUES ($1, $2, NULLIF($3, ''), $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		 ON CONFLICT DO NOTHING`,
		args...,
	); err != nil {
		return BuildArtifactRecord{}, err
	}
	// A fresh direct-image resolution can make the same external image available
	// again. It never resurrects a platform manifest already queued for deletion.
	if _, err := tx.ExecContext(ctx, `UPDATE build_artifacts SET image_retained = TRUE
		WHERE service_id = $1 AND image_ref = $2 AND kind = $3 AND source_image_ref = $4
		AND NOT EXISTS (SELECT 1 FROM registry_image_deletions WHERE image_ref = $2)`,
		rec.ServiceID, rec.ImageRef, BuildArtifactDirectImage, rec.SourceImageRef); err != nil {
		return BuildArtifactRecord{}, err
	}
	return scanBuildArtifactRow(tx.QueryRowContext(ctx,
		`SELECT `+buildArtifactSelectColumns+`
		   FROM build_artifacts
		  WHERE service_id = $1 AND image_ref = $2 AND kind = $3 AND source_image_ref = $4`,
		rec.ServiceID, rec.ImageRef, BuildArtifactDirectImage, rec.SourceImageRef,
	))
}

func (s *persistence) buildArtifactByIDQuerier(ctx context.Context, q ServiceQueryer, artifactID string) (BuildArtifactRecord, error) {
	return scanBuildArtifactRow(q.QueryRowContext(ctx,
		`SELECT `+buildArtifactSelectColumns+`
		   FROM build_artifacts
		  WHERE id = $1`,
		artifactID,
	))
}

// buildArtifactByReuseKeyTx finds the artifact an earlier build of the same source already
// produced. The reuse key is an index, not unique: rebuilds may report different digests,
// and the newest artifact wins.
func (s *persistence) buildArtifactByReuseKeyTx(ctx context.Context, tx *sql.Tx, serviceID, snapshotDigest string, recipe *platformv1.BuildRecipe) (BuildArtifactRecord, bool, error) {
	if strings.TrimSpace(snapshotDigest) == "" {
		return BuildArtifactRecord{}, false, nil
	}
	rec, err := scanBuildArtifactRow(tx.QueryRowContext(ctx,
		`SELECT `+buildArtifactSelectColumns+`
		   FROM build_artifacts
		  WHERE service_id = $1 AND reuse_key = $2 AND image_retained = TRUE
		  ORDER BY created_at DESC, id DESC
		  LIMIT 1`,
		serviceID, artifactReuseKey(snapshotDigest, recipe),
	))
	if errors.Is(err, sql.ErrNoRows) {
		return BuildArtifactRecord{}, false, nil
	}
	return rec, err == nil, err
}

// ListServiceArtifacts returns the newest immutable artifacts recorded for
// a service, newest first. Service-read authorized.
func (d *Delivery) ListServiceArtifacts(ctx context.Context, user authz.User, serviceID string, limit int32) ([]BuildArtifactRecord, error) {
	if _, err := d.store.authz.AuthorizeService(ctx, user, serviceID, authz.Read); err != nil {
		return nil, err
	}
	return d.store.listBuildArtifacts(ctx, serviceID, limit)
}

func (s *persistence) listBuildArtifacts(ctx context.Context, serviceID string, limit int32) ([]BuildArtifactRecord, error) {
	queryLimit := int(limit)
	if queryLimit <= 0 {
		queryLimit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+buildArtifactSelectColumns+`
		   FROM build_artifacts
		  WHERE service_id = $1
		  ORDER BY created_at DESC, id DESC
		  LIMIT $2`,
		serviceID, queryLimit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BuildArtifactRecord
	for rows.Next() {
		rec, err := scanBuildArtifactRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// preResolveDirectImage pins a creation spec's direct image, or empty when it has none.
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

// errDirectImageChanged aborts a deploy whose spec raced tag resolution. The caller
// retries with a fresh pre-read instead of deploying a stale tag.
var errDirectImageChanged = errors.New("direct image changed during release")

// resolvedDirectImage pairs a pre-read direct-image input with the pinned resolution the
// release deploys. keepStored means the input didn't change: no registry consult.
type resolvedDirectImage struct {
	input      string
	resolved   registry.ResolvedImage
	keepStored bool
}

// preResolveManagedImage pins a managed spec's direct image pre-transaction. An unchanged
// spec keeps its stored artifact, so reconciliation never blocks on registry I/O.
func (d *Delivery) preResolveManagedImage(ctx context.Context, projectID, name string, spec *platformv1.ServiceSpec) (resolvedDirectImage, error) {
	input := strings.TrimSpace(directImageRef(spec))
	if input == "" {
		return resolvedDirectImage{}, nil
	}
	current, found, err := d.store.managedServiceByName(ctx, projectID, name)
	if err != nil {
		return resolvedDirectImage{}, err
	}
	if found && current.ResolvedArtifactID != "" && sameServiceSpec(current.Spec, CanonicalServiceSpec(spec)) {
		return resolvedDirectImage{input: input, keepStored: true}, nil
	}
	pinned, err := d.resolveOneDirectImage(ctx, input)
	if err != nil {
		return resolvedDirectImage{}, err
	}
	return resolvedDirectImage{input: input, resolved: pinned}, nil
}

// pendingDirectImageInputs lists the current direct-image refs of live services the release
// will actually select, without locks. Unchanged services' tags are never resolved — a stale
// image must not block unrelated changes — and the transaction re-verifies each input.
func (s *persistence) pendingDirectImageInputs(ctx context.Context, env authz.Environment) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT s.id, rev.spec_json
		   FROM services s
		   JOIN service_delivery_status ds ON ds.service_id = s.id
		   JOIN service_revisions rev ON rev.service_id = s.id AND rev.spec_revision = s.current_spec_revision
		   LEFT JOIN service_rollouts ro ON ro.service_id = s.id AND ro.rollout_generation = ds.current_rollout_generation
		  WHERE s.environment_id = $1
		    AND s.deleted_at IS NULL
		    AND (ds.current_rollout_generation IS NULL OR ro.spec_revision IS DISTINCT FROM s.current_spec_revision)`,
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

// directImageArtifactTx records the pre-resolved digest as this service's artifact, reusing
// the row for an already-pinned digest. The spec must still match the pre-read: races retry
// rather than deploying a stale tag.
func (d *Delivery) directImageArtifactTx(ctx context.Context, tx *sql.Tx, serviceID, input string, pre resolvedDirectImage, actor deploymentActor, now time.Time) (BuildArtifactRecord, error) {
	if strings.TrimSpace(input) == "" {
		return BuildArtifactRecord{}, errors.New("direct image is required")
	}
	if pre.keepStored || pre.resolved.Ref == "" || pre.input != strings.TrimSpace(input) {
		return BuildArtifactRecord{}, errDirectImageChanged
	}
	// The proto keeps source_image_ref for unpinned input only: an already-pinned reference
	// must not round-trip as mutable user input.
	sourceImageRef := strings.TrimSpace(input)
	if parsed, err := registry.ParseReference(sourceImageRef); err == nil && parsed.Pinned() {
		sourceImageRef = ""
	}
	return d.store.insertDirectImageArtifactTx(ctx, tx, insertArtifactParams{
		ServiceID:           serviceID,
		Kind:                BuildArtifactDirectImage,
		ImageRepository:     pre.resolved.Repository,
		ImageManifestDigest: pre.resolved.ManifestDigest,
		SourceImageRef:      sourceImageRef,
		BuildActorKind:      actor.Kind,
		BuildActorID:        actor.ID,
		CreatedAt:           now,
	})
}
