package recovery

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

func (s Service) Publish(ctx context.Context, p Point) (Object, Report, error) {
	p.CompletedAt = time.Now().UTC()
	p.ExpiresAt = p.Snapshot.Timestamp.Add(Retention)
	if err := p.Validate(p.CompletedAt); err != nil {
		return Object{}, Report{}, err
	}

	missing := Report{Timestamp: p.Snapshot.Timestamp}
	for _, need := range p.Requirements() {
		d, err := s.Find(ctx, need, p.ExpiresAt)
		if err != nil {
			missing.Missing = append(missing.Missing, identity(need))
			missing.Failures = append(missing.Failures, err.Error())
			continue
		}
		p.Dependencies = append(p.Dependencies, d)
	}
	if len(missing.Missing) > 0 {
		return Object{}, missing, fmt.Errorf("recovery point is missing protected dependencies")
	}

	for n, o := range p.Database.Objects {
		if err := s.Storage.Retain(ctx, o, p.ExpiresAt); err != nil {
			return Object{}, Report{}, err
		}
		current, err := s.Storage.Inspect(ctx, o)
		if err != nil {
			return Object{}, Report{}, err
		}
		p.Database.Objects[n].RetainUntil = current.RetainUntil
	}
	r := s.Verify(ctx, p, true)
	if !r.Complete {
		return Object{}, r, fmt.Errorf("recovery point is incomplete")
	}
	key := s.Prefix + "/points/" + p.Installation + "/" + p.Snapshot.Timestamp.UTC().Format("20060102T150405.000000000") + "/" + strings.TrimPrefix(Digest(jsonBytes(p)), "sha256:")
	o, err := s.putJSON(ctx, key, p, p.ExpiresAt)
	return o, r, err
}

func (s Service) ReadPoint(ctx context.Context, o Object) (Point, error) {
	var p Point
	if o.Version == "" || o.Key == "" || !digestPattern.MatchString(o.Digest) {
		return p, fmt.Errorf("recovery point requires an exact protected manifest version and digest")
	}
	if err := s.verifyObject(ctx, o, time.Now(), true); err != nil {
		return p, err
	}
	if err := s.readJSON(ctx, o, &p); err != nil {
		return p, err
	}
	return p, nil
}

// ResolvePoint accepts only a version-pinned URI in this recovery bucket and
// catalog. Mutable object names cannot select a restoration.
func (s Service) ResolvePoint(ctx context.Context, bucket, raw string) (Object, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "s3" || u.Host != bucket || u.User != nil || u.Fragment != "" || len(u.Query()) != 1 || len(u.Query()["versionId"]) != 1 {
		return Object{}, fmt.Errorf("recovery selection requires a version-pinned S3 catalog URL")
	}
	o := Object{Key: strings.TrimPrefix(u.Path, "/"), Version: u.Query().Get("versionId")}
	if !strings.HasPrefix(o.Key, s.Prefix+"/points/") || o.Version == "" || o.Version == "null" {
		return Object{}, fmt.Errorf("recovery selection is outside the protected point catalog")
	}
	return s.Storage.Inspect(ctx, o)
}

func (s Service) Points(ctx context.Context, installation string) ([]Point, error) {
	prefix := s.Prefix + "/points/"
	if installation != "" {
		prefix += installation + "/"
	}
	versions, err := s.Storage.Versions(ctx, prefix)
	if err != nil {
		return nil, err
	}
	var points []Point
	for _, o := range versions {
		var p Point
		current, err := s.Storage.Inspect(ctx, o)
		if err != nil {
			return nil, err
		}
		o.Digest = current.Digest
		if err := s.readJSON(ctx, o, &p); err != nil {
			return nil, err
		}
		if current.RetainUntil.Before(p.ExpiresAt) && time.Now().Before(p.ExpiresAt) {
			return nil, fmt.Errorf("complete point manifest has insufficient retention")
		}
		if installation != "" && p.Installation != installation {
			return nil, fmt.Errorf("catalog installation mismatch")
		}
		if time.Now().Before(p.ExpiresAt) {
			points = append(points, p)
		}
	}
	sort.Slice(points, func(i, j int) bool { return points[i].Snapshot.Timestamp.After(points[j].Snapshot.Timestamp) })
	return points, nil
}

// CheckFreshness runs from an independent monitor. It never advances freshness
// for an incomplete point or for the time at which verification finished.
func (s Service) CheckFreshness(ctx context.Context, installation string) (Report, error) {
	points, err := s.Points(ctx, installation)
	if err != nil {
		return Report{}, err
	}
	r := Report{}
	for _, p := range points {
		candidate := s.Verify(ctx, p, false)
		if candidate.Complete {
			candidate.AgeSeconds = time.Since(p.Snapshot.Timestamp).Seconds()
			candidate.Failures = append(candidate.Failures, r.Failures...)
			candidate.Missing = append(candidate.Missing, r.Missing...)
			attempt, err := s.LatestAttempt(ctx, installation)
			if err != nil {
				return candidate, err
			}
			if attempt.At.After(p.CompletedAt) {
				candidate.Failures = append(candidate.Failures, attempt.Report.Failures...)
				candidate.Missing = append(candidate.Missing, attempt.Report.Missing...)
			}
			if time.Since(p.Snapshot.Timestamp) > Objective {
				return candidate, fmt.Errorf("latest complete recovery point exceeds the 15-minute objective")
			}
			return candidate, nil
		}
		r.Missing = append(r.Missing, candidate.Missing...)
		r.Failures = append(r.Failures, candidate.Failures...)
	}
	return r, fmt.Errorf("no retained complete recovery point")
}
