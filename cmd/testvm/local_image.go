package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const (
	localBaseImageName       = "noble-server-cloudimg-amd64.img"
	localPreparedImagePrefix = "noble-prepared-"
	// localImageBuilderVersion invalidates every cached prepared image when the
	// build procedure changes, independently of its inputs. It does not capture
	// qemu/virt-customize or apt package versions; bump it when those matter.
	localImageBuilderVersion = 2
)

var (
	localBaseImageDir = "https://cloud-images.ubuntu.com/noble/current/"
)

func localBaseImageURL() string { return localBaseImageDir + localBaseImageName }
func localChecksumsURL() string { return localBaseImageDir + "SHA256SUMS" }

// localImagePackages is the single source of truth for what the local prepared
// image contains. It is a superset of every role's cloud-init package list; a
// test enforces that the cloud-init templates never drift from it.
var localImagePackages = []string{
	"containerd",
	"containernetworking-plugins",
	"iproute2",
	"iptables",
	"wireguard-tools",
	"jq",
	"curl",
	"python3",
}

var localImageServices = []string{"containerd"}

// imageRecipe is the complete input to a prepared image. Its key is the cache
// identity: any change to the base image, package set, services, apt mirror, or
// builder procedure produces a different image instead of silently reusing a
// stale one.
type imageRecipe struct {
	BaseSHA   string   `json:"base_sha"`
	Packages  []string `json:"packages"`
	Services  []string `json:"services"`
	AptMirror string   `json:"apt_mirror,omitempty"`
	Builder   int      `json:"builder"`
}

func localImageRecipe(baseSHA, aptMirror string) imageRecipe {
	return imageRecipe{
		BaseSHA:   strings.ToLower(strings.TrimSpace(baseSHA)),
		Packages:  slices.Clone(localImagePackages),
		Services:  slices.Clone(localImageServices),
		AptMirror: strings.TrimRight(strings.TrimSpace(aptMirror), "/"),
		Builder:   localImageBuilderVersion,
	}
}

func (r imageRecipe) key() string {
	payload, err := json.Marshal(r)
	if err != nil {
		panic(fmt.Sprintf("marshal image recipe: %v", err))
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])[:16]
}

type imageMeta struct {
	Key       string    `json:"key"`
	BaseSHA   string    `json:"base_sha"`
	Packages  []string  `json:"packages"`
	Services  []string  `json:"services"`
	AptMirror string    `json:"apt_mirror,omitempty"`
	Builder   int       `json:"builder"`
	CreatedAt time.Time `json:"created_at"`
}

// imageBuilder turns a base image and recipe into a prepared image at dest.
type imageBuilder interface {
	Build(ctx context.Context, recipe imageRecipe, base, dest string) error
}

// virtCustomizeBuilder bakes the recipe into a copy of the base image with
// libguestfs, without ever booting the guest: no sshd wait, no cloud-init, no
// poweroff. The prepared image also has its identity reset so every VM cloned
// from it gets a fresh machine-id and host keys.
type virtCustomizeBuilder struct{}

func (virtCustomizeBuilder) Build(ctx context.Context, recipe imageRecipe, base, dest string) error {
	if out, err := exec.CommandContext(ctx, "qemu-img", "convert", "-O", "qcow2", base, dest).CombinedOutput(); err != nil {
		return fmt.Errorf("copy base image: %w: %s", err, strings.TrimSpace(string(out)))
	}
	args, err := virtCustomizeArgs(recipe, dest)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "virt-customize", args...)
	cmd.Env = append(os.Environ(), "LIBGUESTFS_BACKEND=direct")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("virt-customize: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func virtCustomizeArgs(recipe imageRecipe, dest string) ([]string, error) {
	install := "DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends " + strings.Join(recipe.Packages, " ")
	args := []string{"-a", dest, "--network"}
	if recipe.AptMirror != "" {
		if err := validateAptMirror(recipe.AptMirror); err != nil {
			return nil, err
		}
		// Mirror is validated to a URL charset that cannot break this sed, then
		// passed as a sed replacement (not interpolated into an unquoted shell).
		rewrite := "sed -i -E 's#https?://(archive|security)\\.ubuntu\\.com/ubuntu#" + recipe.AptMirror + "#g' /etc/apt/sources.list.d/ubuntu.sources 2>/dev/null || true"
		args = append(args, "--run-command", rewrite)
	}
	args = append(args, "--run-command", "apt-get update", "--run-command", install)
	// Do not use `systemctl enable` in the libguestfs chroot; it can no-op.
	// Pin the multi-user wants symlink so cloned VMs start containerd.
	args = append(args, "--mkdir", "/etc/systemd/system/multi-user.target.wants")
	for _, service := range recipe.Services {
		unit := service + ".service"
		args = append(args, "--link", "/lib/systemd/system/"+unit+":/etc/systemd/system/multi-user.target.wants/"+unit)
	}
	args = append(args,
		"--run-command", "apt-get clean",
		"--run-command", "rm -rf /var/lib/apt/lists/*",
		"--run-command", "rm -f /var/lib/systemd/random-seed",
		"--run-command", "rm -f /etc/ssh/ssh_host_*",
		"--truncate", "/etc/machine-id",
		"--truncate", "/var/lib/dbus/machine-id",
	)
	return args, nil
}

// imageManager owns the prepared-image cache: provenance checks, validation,
// and atomic publication. The builder and validator are injected so the cache
// logic is exercised without libguestfs.
type imageManager struct {
	mu       sync.Mutex
	cacheDir string
	builder  imageBuilder
	validate func(context.Context, string) error
	now      func() time.Time
}

func newLocalImageManager(cacheDir string) *imageManager {
	return &imageManager{
		cacheDir: cacheDir,
		builder:  virtCustomizeBuilder{},
		validate: qemuImgCheck,
		now:      func() time.Time { return time.Now().UTC() },
	}
}

// ensure returns a prepared image for the given base image and recipe, building
// it only when the cached one is missing, has different inputs, or fails
// validation.
func (m *imageManager) ensure(ctx context.Context, basePath string, recipe imageRecipe) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := recipe.key()
	dest := filepath.Join(m.cacheDir, localPreparedImagePrefix+key+".qcow2")
	metaPath := dest + ".json"
	if err := os.MkdirAll(m.cacheDir, 0o755); err != nil {
		return "", err
	}
	lock, err := acquireImageCacheLock(ctx, filepath.Join(m.cacheDir, ".prepared-image.lock"))
	if err != nil {
		return "", err
	}
	defer func() {
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		_ = lock.Close()
	}()
	if ok, err := m.reusable(ctx, dest, metaPath, key); ok {
		infof("local: using prepared image %s", dest)
		return dest, nil
	} else if err != nil {
		if !cacheRebuildable(err) {
			return "", fmt.Errorf("cached image %s: %w", dest, err)
		}
		infof("local: cached image %s is unusable (%v); rebuilding", dest, err)
	}
	tempFile, err := os.CreateTemp(m.cacheDir, "."+filepath.Base(dest)+"-*.part")
	if err != nil {
		return "", err
	}
	temp := tempFile.Name()
	if err := tempFile.Close(); err != nil {
		_ = os.Remove(temp)
		return "", err
	}
	defer os.Remove(temp)
	infof("local: building prepared image %s (one-time package install)", dest)
	if err := m.builder.Build(ctx, recipe, basePath, temp); err != nil {
		_ = os.Remove(temp)
		return "", fmt.Errorf("build prepared image: %w", err)
	}
	if err := m.validate(ctx, temp); err != nil {
		_ = os.Remove(temp)
		return "", fmt.Errorf("validate prepared image: %w", err)
	}
	if err := os.Rename(temp, dest); err != nil {
		_ = os.Remove(temp)
		return "", err
	}
	meta := imageMeta{
		Key:       key,
		BaseSHA:   recipe.BaseSHA,
		Packages:  recipe.Packages,
		Services:  recipe.Services,
		AptMirror: recipe.AptMirror,
		Builder:   recipe.Builder,
		CreatedAt: m.now(),
	}
	if err := writeImageMeta(metaPath, meta); err != nil {
		_ = os.Remove(dest)
		return "", err
	}
	infof("local: prepared image ready at %s", dest)
	return dest, nil
}

func acquireImageCacheLock(ctx context.Context, path string) (*os.File, error) {
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			_ = lock.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = lock.Close()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (m *imageManager) reusable(ctx context.Context, dest, metaPath, key string) (bool, error) {
	info, err := os.Stat(dest)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if info.Size() == 0 {
		return false, errors.New("image is empty")
	}
	meta, err := readImageMeta(metaPath)
	if err != nil {
		return false, err
	}
	if meta.Key != key {
		return false, fmt.Errorf("recipe changed: cached %s want %s", meta.Key, key)
	}
	if err := m.validate(ctx, dest); err != nil {
		return false, err
	}
	return true, nil
}

// cacheRebuildable reports whether a reusable() error is a stale/missing cache
// rather than a hard I/O or permission failure that should abort.
func cacheRebuildable(err error) bool {
	if err == nil || os.IsNotExist(err) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if os.IsPermission(err) {
		return false
	}
	var pe *os.PathError
	if errors.As(err, &pe) {
		return os.IsNotExist(pe.Err)
	}
	return true
}

func qemuImgCheck(ctx context.Context, path string) error {
	out, err := exec.CommandContext(ctx, "qemu-img", "check", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-img check: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func writeImageMeta(path string, meta imageMeta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

func readImageMeta(path string) (imageMeta, error) {
	var meta imageMeta
	data, err := os.ReadFile(path)
	if err != nil {
		return meta, err
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, err
	}
	return meta, nil
}

func writeFileAtomic(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

// ensureLocalBaseImage returns the SHA-256 digest of path. When managed is
// false the file is an explicit -local-base-image: it is hashed in place and
// never fetched or overwritten. When managed is true the default cache file is
// verified against the current upstream SHA256SUMS and downloaded on miss.
func ensureLocalBaseImage(ctx context.Context, path string, managed bool) (string, error) {
	if !managed {
		actual, err := sha256File(path)
		if err != nil {
			return "", fmt.Errorf("local-base-image %s: %w", path, err)
		}
		infof("local: using explicit base image %s", path)
		return actual, nil
	}
	expected, err := fetchLocalChecksum(ctx)
	if err != nil {
		return "", err
	}
	if actual, err := sha256File(path); err == nil && actual == expected {
		infof("local: using cached base image %s", path)
		return expected, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	infof("local: downloading base image %s", localBaseImageURL())
	tempFile, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.part")
	if err != nil {
		return "", err
	}
	temp := tempFile.Name()
	if err := tempFile.Close(); err != nil {
		_ = os.Remove(temp)
		return "", err
	}
	defer os.Remove(temp)
	if err := downloadLocalBaseImage(ctx, localBaseImageURL(), temp); err != nil {
		os.Remove(temp)
		return "", err
	}
	actual, err := sha256File(temp)
	if err != nil {
		os.Remove(temp)
		return "", err
	}
	if actual != expected {
		os.Remove(temp)
		return "", fmt.Errorf("base image checksum mismatch: got %s want %s", actual, expected)
	}
	if err := os.Rename(temp, path); err != nil {
		os.Remove(temp)
		return "", err
	}
	infof("local: base image verified at %s", path)
	return expected, nil
}

func fetchLocalChecksum(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, localChecksumsURL(), nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch base image checksum: status %s", resp.Status)
	}
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || len(fields[0]) != 64 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name != localBaseImageName {
			continue
		}
		if _, err := hex.DecodeString(fields[0]); err != nil {
			return "", errors.New("malformed base image checksum value")
		}
		return strings.ToLower(fields[0]), nil
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("checksum for %s not found in %s", localBaseImageName, localChecksumsURL())
}

func downloadLocalBaseImage(ctx context.Context, url, destination string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download base image: status %s", resp.Status)
	}
	file, err := os.Create(destination)
	if err != nil {
		return err
	}
	defer file.Close()
	written, err := io.Copy(file, resp.Body)
	if err != nil {
		return err
	}
	infof("local: downloaded %d bytes", written)
	return file.Sync()
}

func sha256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
