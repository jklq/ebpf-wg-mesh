package recovery

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"ebof-wg-mesh/internal/controlplane/source"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type DrillReport struct {
	Verified               bool      `json:"verified"`
	DeclaredAt             time.Time `json:"declaredAt"`
	AvailableAt            time.Time `json:"availableAt"`
	ElapsedSeconds         float64   `json:"elapsedSeconds"`
	FleetSize              int       `json:"fleetSize"`
	DataBytes              int64     `json:"dataBytes"`
	TransferBytesPerSecond float64   `json:"transferBytesPerSecond"`
	CapacityAssumptions    string    `json:"capacityAssumptions"`
	WithinTwoHours         bool      `json:"withinTwoHours"`
}
type drillInput struct {
	Config          Config            `json:"config"`
	Point           Point             `json:"point"`
	Files           map[string]string `json:"files"`
	Workspace       string            `json:"workspace"`
	ParentNetworkNS string            `json:"parentNetworkNS"`
}
type DrillReady struct {
	DatabaseURL     string   `json:"databaseURL"`
	KeyringFile     string   `json:"keyringFile"`
	ConsoleKeyFiles []string `json:"consoleKeyFiles"`
	Registry        string   `json:"registry"` // loopback host:port, original repository paths
	AuthFile        string   `json:"authFile"`
	AvailabilityURL string   `json:"availabilityURL"`
	SourceDirectory string   `json:"sourceDirectory"`
	RegistryCertDir string   `json:"registryCertDir"`
}

func (s Service) Drill(ctx context.Context, c Config, p Point, declared time.Time) (r DrillReport, returnErr error) {
	r = DrillReport{DeclaredAt: declared, FleetSize: c.FleetSize, CapacityAssumptions: c.CapacityAssumptions}
	if declared.IsZero() || declared.After(time.Now()) || c.FleetSize < 1 || c.CapacityAssumptions == "" || len(c.DrillCommand) == 0 {
		return r, fmt.Errorf("drill requires declaration time, tested fleet size, capacity assumptions, and release recovery tool")
	}
	ctx, cancel := context.WithDeadline(ctx, declared.Add(2*time.Hour))
	defer cancel()
	defer func() {
		if !r.Verified {
			r.ElapsedSeconds = time.Since(declared).Seconds()
		}
	}()
	digest := c.DrillCommandDigest
	pinned := false
	for _, d := range p.Dependencies {
		if d.Kind == "tool" && d.Digest == digest {
			pinned = true
		}
	}
	if !pinned {
		return r, fmt.Errorf("selected recovery point does not contain this recovery executable")
	}
	dir, err := os.MkdirTemp("", "platform-restore-*")
	if err != nil {
		return r, err
	}
	defer os.RemoveAll(dir)
	input := drillInput{Config: c, Point: p, Files: map[string]string{}, Workspace: dir}
	input.ParentNetworkNS, err = os.Readlink("/proc/self/ns/net")
	if err != nil {
		return r, err
	}
	transferStart := time.Now()
	materialize := func(o Object, name string) error {
		if filepath.IsAbs(name) || filepath.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("protected object inventory escapes the restore workspace")
		}
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := s.Storage.Get(ctx, o, path); err != nil {
			return err
		}
		if err := os.Chmod(path, 0600); err != nil {
			return err
		}
		digest, size, err := FileDigest(path)
		if err != nil {
			return err
		}
		if digest != o.Digest || size != o.Size {
			return fmt.Errorf("restore transfer failed its protected digest")
		}
		r.DataBytes += size
		return nil
	}
	for _, o := range p.Database.Objects {
		if err := materialize(o, "database/"+o.Key); err != nil {
			return r, err
		}
	}
	for _, d := range p.Dependencies {
		if len(d.Objects) != 1 {
			return r, fmt.Errorf("unsupported multi-object dependency restore")
		}
		name := "dependencies/" + strings.TrimPrefix(Digest([]byte(d.Kind+"/"+d.ID)), "sha256:")
		if err := materialize(d.Objects[0], name); err != nil {
			return r, err
		}
		path := filepath.Join(dir, name)
		if d.Kind == "keyring" || d.Kind == "console-key" || d.Kind == "external-secret" || d.Kind == "deployment-state" {
			b, err := os.ReadFile(path)
			if err != nil {
				return r, err
			}
			plain, err := s.open(d.Kind, d.ID, b)
			if err != nil {
				return r, err
			}
			err = os.WriteFile(path, plain, 0600)
			clear(plain)
			if err != nil {
				return r, err
			}
		}
		if d.Kind == "tool" {
			if err := os.Chmod(path, 0700); err != nil {
				return r, err
			}
			if d.Digest == c.DrillCommandDigest {
				input.Config.DrillCommand[0] = path
			}
		}
		input.Files[d.Kind+"/"+d.ID] = path
	}
	r.TransferBytesPerSecond = float64(r.DataBytes) / time.Since(transferStart).Seconds()
	// No storage/provider credentials enter the isolated validator's environment.
	input.Config.Storage = StorageConfig{}
	input.Config.RecoveryKeyFile = ""
	input.Config.DatabaseURLFile = ""
	input.Config.KeyringFile = ""
	input.Config.Images.AuthFile = ""
	input.Config.Images.CertificateDirectory = ""
	for n := range input.Config.Files {
		f := &input.Config.Files[n]
		if f.Path == c.Images.Binary {
			input.Config.Images.Binary = input.Files[identity(f.Requirement)]
		}
		f.Path = input.Files[identity(f.Requirement)]
	}
	path := filepath.Join(dir, "input.json")
	if err := os.WriteFile(path, jsonBytes(input), 0600); err != nil {
		return r, err
	}
	binary, err := os.Executable()
	if err != nil {
		return r, err
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/unshare", "--net", "--pid", "--fork", "--mount-proc", "--kill-child", binary, "recovery-isolated", path)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "PLATFORM_RECOVERY_WORKSPACE=" + dir}
	if err := cmd.Run(); err != nil {
		return r, fmt.Errorf("isolated recovery failed (namespace privileges and offline release tool are required): %w", err)
	}
	r.Verified = true
	r.AvailableAt = time.Now().UTC()
	r.ElapsedSeconds = r.AvailableAt.Sub(declared).Seconds()
	r.WithinTwoHours = r.ElapsedSeconds <= 7200
	if !r.WithinTwoHours {
		return r, fmt.Errorf("verified recovery exceeded the two-hour target")
	}
	return r, nil
}

// RunIsolated validates natively restored data inside the namespace created by
// Drill. It accepts only loopback endpoints and files under the fresh workspace.
func RunIsolated(ctx context.Context, inputPath string) error {
	b, err := os.ReadFile(inputPath)
	if err != nil {
		return err
	}
	var input drillInput
	if err := json.Unmarshal(b, &input); err != nil {
		return err
	}
	if err := prepareIsolation(input.ParentNetworkNS); err != nil {
		return err
	}
	if err := mountOfflineWorkspace(input.Workspace); err != nil {
		return err
	}
	c := input.Config
	if len(c.DrillCommand) == 0 {
		return fmt.Errorf("offline recovery executable is missing")
	}
	cmd := exec.CommandContext(ctx, c.DrillCommand[0], append(c.DrillCommand[1:], input.Workspace, inputPath)...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "PLATFORM_RECOVERY_WORKSPACE=" + input.Workspace}
	raw, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("offline release provisioning/restore failed: %w", err)
	}
	var ready DrillReady
	if err := json.Unmarshal(raw, &ready); err != nil {
		return fmt.Errorf("offline recovery tool must return DrillReady JSON")
	}
	loopback := func(hostport string) bool {
		host, _, err := net.SplitHostPort(hostport)
		ip := net.ParseIP(host)
		return err == nil && ip != nil && ip.IsLoopback()
	}
	u, err := url.Parse(ready.DatabaseURL)
	if err != nil || !loopback(u.Host) {
		return fmt.Errorf("isolated database must use a literal loopback address")
	}
	available, err := url.Parse(ready.AvailabilityURL)
	if err != nil || !loopback(available.Host) || (available.Scheme != "http" && available.Scheme != "https") {
		return fmt.Errorf("isolated platform availability requires a loopback endpoint")
	}
	if !loopback(ready.Registry) {
		return fmt.Errorf("isolated registry must use a literal loopback address")
	}
	workspace, err := filepath.EvalSymlinks(input.Workspace)
	if err != nil {
		return fmt.Errorf("resolve recovery workspace: %w", err)
	}
	inside := func(file string) bool {
		resolved, err := filepath.EvalSymlinks(file)
		if err != nil {
			return false
		}
		rel, err := filepath.Rel(workspace, resolved)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
	}
	if !inside(ready.KeyringFile) || !inside(ready.AuthFile) {
		return fmt.Errorf("isolated recovery credentials must reside in its workspace")
	}
	if ready.RegistryCertDir != "" && !inside(ready.RegistryCertDir) {
		return fmt.Errorf("isolated registry certificates are outside its workspace")
	}
	for _, f := range ready.ConsoleKeyFiles {
		if !inside(f) {
			return fmt.Errorf("isolated console key is outside its workspace")
		}
	}
	db, err := sql.Open("pgx", ready.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := CheckRestoredDatabase(ctx, db, input.Point, c.ConsoleSchema, ready.KeyringFile, ready.ConsoleKeyFiles); err != nil {
		return err
	}
	if err := checkRestoredSources(input.Point, ready.SourceDirectory, input.Workspace); err != nil {
		return err
	}
	imageTool := ""
	for _, d := range input.Point.Dependencies {
		if d.Kind == "tool" && strings.HasSuffix(d.ID, "/tool/skopeo/"+runtime.GOARCH) {
			imageTool = input.Files[identity(Requirement{Kind: d.Kind, ID: d.ID})]
		}
	}
	for _, d := range input.Point.Dependencies {
		if d.Kind != "image" {
			continue
		}
		if imageTool == "" {
			return fmt.Errorf("selected recovery point lacks the registry recovery executable")
		}
		dir, err := os.MkdirTemp(input.Workspace, "image-*")
		if err != nil {
			return err
		}
		if err := extractOCI(input.Files[d.Kind+"/"+d.ID], dir); err != nil {
			return err
		}
		inventory, err := InspectOCI(dir, d.Digest)
		if err != nil {
			return err
		}
		if string(jsonBytes(inventory)) != string(jsonBytes(d.Inventory)) {
			return fmt.Errorf("restored image digest inventory mismatch")
		}
		repo := strings.SplitN(d.ID, "/", 2)
		if len(repo) != 2 {
			return fmt.Errorf("invalid image repository")
		}
		destination := ready.Registry + "/" + strings.Split(repo[1], "@")[0]
		images := Images{Binary: imageTool, AuthFile: ready.AuthFile, CertificateDirectory: ready.RegistryCertDir}
		if err := images.copy(ctx, "oci:"+dir+":recovery", "docker://"+destination+":recovery-"+strings.TrimPrefix(d.Digest, "sha256:")); err != nil {
			return err
		}
		raw, err := images.inspect(ctx, destination+"@"+d.Digest)
		if err != nil || Digest(raw) != d.Digest {
			return fmt.Errorf("restored image cannot be retrieved by its original digest")
		}
	}
	client := http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("availability redirects are forbidden") }}
	request, err := http.NewRequestWithContext(ctx, "GET", ready.AvailabilityURL, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("restored platform is unavailable")
	}
	return nil
}

func checkRestoredSources(p Point, directory, workspace string) error {
	workspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return fmt.Errorf("resolve recovery workspace: %w", err)
	}
	for _, d := range p.Dependencies {
		if d.Kind != "source" {
			continue
		}
		identity, err := source.DigestFromObjectKey(d.ID)
		if err != nil || identity != d.Digest {
			return fmt.Errorf("invalid restored source identity")
		}
		path, err := filepath.EvalSymlinks(filepath.Join(directory, filepath.FromSlash(d.ID)))
		if err != nil {
			return fmt.Errorf("restored source archive %s is unavailable: %w", d.ID, err)
		}
		rel, err := filepath.Rel(workspace, path)
		if directory == "" || err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return fmt.Errorf("restored source archive is outside the isolated workspace")
		}
		digest, size, err := FileDigest(path)
		if err != nil || len(d.Objects) != 1 || digest != d.Digest || size != d.Objects[0].Size {
			return fmt.Errorf("restored source archive %s failed content verification", d.ID)
		}
	}
	return nil
}
