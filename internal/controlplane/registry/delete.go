package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ebof-wg-mesh/internal/config"
)

// Deleter deletes only build repositories in the operator's platform registry.
// It cannot delete direct images in a user's external registry.
type Deleter struct {
	policy *Policy
	auth   *Auth
	client *http.Client
}

func NewDeleter(cfg config.RegistryConfig, auth *Auth) *Deleter {
	return &Deleter{policy: NewPolicy(cfg, auth), auth: auth, client: &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("registry delete redirect refused") },
	}}
}

func (d *Deleter) DeleteImage(ctx context.Context, ref string) error {
	deleteImage, err := d.PrepareDelete(ctx, ref)
	if err != nil {
		return err
	}
	return deleteImage(ctx)
}

// PrepareDelete mints database-backed auth before the caller locks its deletion
// queue. The returned idempotent operation performs only registry I/O.
func (d *Deleter) PrepareDelete(ctx context.Context, ref string) (func(context.Context) error, error) {
	parsed, err := ParseReference(ref)
	if err != nil || !parsed.Pinned() {
		return nil, fmt.Errorf("registry deletion requires a digest-pinned image: %s", ref)
	}
	repository, err := d.policy.repositoryForReference(ref)
	if err != nil {
		return nil, err
	}
	path := repository
	if d.policy.namespacePrefix != "" {
		prefix := d.policy.namespacePrefix + "/"
		if !strings.HasPrefix(path, prefix) {
			return nil, errors.New("registry deletion is outside the platform namespace")
		}
		path = strings.TrimPrefix(path, prefix)
	}
	if len(strings.Split(path, "/")) != 4 {
		return nil, errors.New("registry deletion requires a platform build repository")
	}
	token, err := d.auth.deletionToken(ctx, repository)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, manifestURLFor(parsed.Repository, parsed.Digest), nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := d.client.Do(req)
		if err != nil {
			return fmt.Errorf("delete registry manifest: %w", err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNotFound {
			return nil
		}
		return fmt.Errorf("delete registry manifest %s: HTTP %d; enable storage.delete.enabled in the registry", ref, resp.StatusCode)
	}, nil
}
