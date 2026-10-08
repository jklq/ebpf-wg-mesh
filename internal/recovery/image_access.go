package recovery

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/controlplane/registry"
	"ebof-wg-mesh/internal/controlplane/secretkeys"
	"ebof-wg-mesh/internal/controlplane/signkeys"
)

// ImageAccess uses the current native signing authority for an exact platform
// repository. External registry credentials stay in the selected auth file.
// No credential from a lost authority is reused after recovery.
func ImageAccess(ctx context.Context, db *sql.DB, keyring string, images Images, ref string, push bool) (Images, func(), error) {
	cleanup := func() {}
	repository := strings.Split(ref, "@")[0]
	if images.RegistryService == "" || strings.Split(repository, "/")[0] != images.RegistryService {
		return images, cleanup, nil
	}
	name := strings.TrimPrefix(repository, images.RegistryService+"/")
	if name == repository || name == "" {
		return images, cleanup, fmt.Errorf("registry image requires an exact repository")
	}
	keys, err := secretkeys.Open(ctx, db, config.SecretKeysConfig{KeyringPath: keyring}, secretkeys.Options{})
	if err != nil {
		return images, cleanup, err
	}
	defer keys.Close()
	dir, err := os.MkdirTemp("", "recovery-registry-access-")
	if err != nil {
		return images, cleanup, err
	}
	cleanup = func() { os.RemoveAll(dir) }
	auth, err := registry.NewAuth(ctx, config.RegistryConfig{Host: images.RegistryService, TokenService: images.RegistryService, TokenIssuer: "ebpf-wg-mesh", CredentialTTLSeconds: 1800}, signkeys.New(db, keys.Registry()), dir)
	if err != nil {
		cleanup()
		return images, func() {}, err
	}
	actions := []string{"pull"}
	if push {
		actions = append(actions, "push")
	}
	expires := time.Now().Add(30 * time.Minute)
	user, password, err := auth.MintCredential(ctx, "independent-recovery", name, actions, &expires)
	if err != nil {
		cleanup()
		return images, func() {}, err
	}
	document, err := json.Marshal(map[string]any{"auths": map[string]any{repository: map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte(user + ":" + password))}}})
	if err != nil {
		cleanup()
		return images, func() {}, err
	}
	images.AuthFile = filepath.Join(dir, "auth.json")
	if err = os.WriteFile(images.AuthFile, document, 0600); err != nil {
		cleanup()
		return images, func() {}, err
	}
	return images, cleanup, nil
}
