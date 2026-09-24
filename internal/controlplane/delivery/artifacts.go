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

	"github.com/google/uuid"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/authz"
	"ebof-wg-mesh/internal/controlplane/source"
)

// BuilderToolchainVersion identifies the platform builder toolchain that
// produced a build artifact. It is part of the reuse key: bumping it when
// the toolchain changes stops skip-rebuild lookups from matching images
// built by older toolchains. Builder fleet skew stays with 7.3; this is a
// platform property, not a per-builder report.
const BuilderToolchainVersion = "v1"

// Build artifact kinds. A build artifact records a successful platform
// build; a direct-image artifact records a tag resolved to a digest at
// deploy time. Both shapes pin runtime identity to a manifest digest.
const (
	BuildArtifactBuild       = "build"
	BuildArtifactDirectImage = "direct_image"
)

// BuildArtifactRecord is the immutable source of runtime identity. Rows
// are insert-only: nothing updates an artifact, and retention deletes
// only artifacts no deployment, rollout, or current pointer references.
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
	reuse_key, build_actor_kind, build_actor_id, created_at`

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

// artifactReuseKey identifies build content for skip-rebuild lookups:
// the verified source snapshot, the canonical recipe, and the toolchain
// version. Recipe defaults match equalDesiredSourceSpec so semantically
// identical recipes converge on one key.
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

// buildArtifactRecordFromParams validates artifact parameters and fills
// the derived identity fields: the artifact id, the digest-pinned image
// ref, and the build reuse key.
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

// insertBuildArtifactTx records the immutable artifact of one successful
// build. Every successful build records its own artifact — provenance
// (commit, actor, build time) belongs to the build that produced the
// image — so two builds that report the same manifest digest keep
// distinct rows. Callers must pass a digest-pinned image: user tags are
// resolved before the product transaction and never reach this insert.
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

// insertDirectImageArtifactTx records a resolved direct image, converging
// concurrent releases on one row per (service, image): re-resolving a tag
// that still points at the same manifest digest records the same fact, so
// earlier releases keep referencing one artifact.
func (s *persistence) insertDirectImageArtifactTx(ctx context.Context, tx *sql.Tx, params insertArtifactParams) (BuildArtifactRecord, error) {
	if params.Kind != BuildArtifactDirectImage {
		return BuildArtifactRecord{}, fmt.Errorf("insertDirectImageArtifactTx requires kind %q, got %q", BuildArtifactDirectImage, params.Kind)
	}
	rec, err := buildArtifactRecordFromParams(params)
	if err != nil {
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

// buildArtifactByReuseKeyTx finds the artifact an earlier build of the
// same source already produced so the new build can be skipped. The
// reuse key is an index, not a uniqueness constraint: a rebuild of one
// source may legitimately report a different manifest digest (builds are
// not bit-reproducible) and every successful build records its own
// artifact. The newest recorded artifact wins the lookup.
func (s *persistence) buildArtifactByReuseKeyTx(ctx context.Context, tx *sql.Tx, serviceID, snapshotDigest string, recipe *platformv1.BuildRecipe) (BuildArtifactRecord, bool, error) {
	if strings.TrimSpace(snapshotDigest) == "" {
		return BuildArtifactRecord{}, false, nil
	}
	rec, err := scanBuildArtifactRow(tx.QueryRowContext(ctx,
		`SELECT `+buildArtifactSelectColumns+`
		   FROM build_artifacts
		  WHERE service_id = $1 AND reuse_key = $2
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

// pruneBuildArtifactsTx deletes unreferenced artifacts older than cutoff
// beyond the newest keepRecent per service. Anything a deployment,
// transition, rollout, or current pointer references is rollback material
// and never pruned, no matter its age.
func (s *persistence) pruneBuildArtifactsTx(ctx context.Context, tx *sql.Tx, cutoff time.Time, keepRecent int) (int64, error) {
	if keepRecent < 0 {
		keepRecent = 0
	}
	result, err := tx.ExecContext(ctx,
		`DELETE FROM build_artifacts WHERE id IN (
			SELECT id FROM (
				SELECT a.id,
				       row_number() OVER (PARTITION BY a.service_id ORDER BY a.created_at DESC, a.id DESC) AS rn
				  FROM build_artifacts a
				 WHERE a.created_at < $1
				   AND NOT EXISTS (SELECT 1 FROM deployments d WHERE d.artifact_id = a.id)
				   AND NOT EXISTS (SELECT 1 FROM deployment_transitions t WHERE t.artifact_id = a.id)
				   AND NOT EXISTS (SELECT 1 FROM service_rollouts r WHERE r.artifact_id = a.id)
				   AND NOT EXISTS (SELECT 1 FROM service_delivery_status s WHERE s.current_artifact_id = a.id)
			) ranked WHERE rn > $2
		)`,
		cutoff, keepRecent,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// PruneBuildArtifacts removes aged-out unreferenced artifacts across all
// services. It reports how many rows were deleted.
func (d *Delivery) PruneBuildArtifacts(ctx context.Context, cutoff time.Time, keepRecent int) (int64, error) {
	s := d.store
	var deleted int64
	err := s.withProductTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err error
		deleted, err = s.pruneBuildArtifactsTx(ctx, tx, cutoff, keepRecent)
		return err
	})
	return deleted, err
}
