package recovery

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Images struct {
	Binary               string `json:"binary"`
	AuthFile             string `json:"authFile"`
	CertificateDirectory string `json:"certificateDirectory,omitempty"`
}
type descriptor struct {
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	MediaType string `json:"mediaType"`
}
type imageManifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	Manifests     []descriptor `json:"manifests"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
}

func (i Images) copy(ctx context.Context, source, target string) error {
	if !filepath.IsAbs(i.Binary) || !filepath.IsAbs(i.AuthFile) {
		return fmt.Errorf("registry recovery requires a pinned skopeo binary and external auth file")
	}
	args := []string{"copy", "--all", "--preserve-digests", "--authfile", i.AuthFile}
	if i.CertificateDirectory != "" {
		args = append(args, "--src-cert-dir", i.CertificateDirectory, "--dest-cert-dir", i.CertificateDirectory)
	}
	cmd := exec.CommandContext(ctx, i.Binary, append(args, source, target)...)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("supported registry export/import failed: %w", err)
	}
	return nil
}

func (i Images) inspect(ctx context.Context, ref string) ([]byte, error) {
	args := []string{"inspect", "--raw", "--authfile", i.AuthFile}
	if i.CertificateDirectory != "" {
		args = append(args, "--cert-dir", i.CertificateDirectory)
	}
	return exec.CommandContext(ctx, i.Binary, append(args, "docker://"+ref)...).Output()
}

// InspectOCI walks every manifest in an index, plus each configuration and layer.
// Digest preservation is mandatory; a missing architecture or blob is fatal.
func InspectOCI(directory, root string) ([]string, error) {
	if !digestPattern.MatchString(root) {
		return nil, fmt.Errorf("invalid image root digest")
	}
	b, err := os.ReadFile(filepath.Join(directory, "index.json"))
	if err != nil {
		return nil, err
	}
	var index imageManifest
	if err := json.Unmarshal(b, &index); err != nil {
		return nil, err
	}
	var top *descriptor
	for n := range index.Manifests {
		if index.Manifests[n].Digest == root {
			top = &index.Manifests[n]
		}
	}
	if top == nil {
		return nil, fmt.Errorf("OCI export does not preserve the requested manifest digest")
	}
	visited := map[string]descriptor{}
	var walk func(descriptor, bool) error
	walk = func(d descriptor, manifest bool) error {
		if !digestPattern.MatchString(d.Digest) || d.Size < 0 || d.MediaType == "" {
			return fmt.Errorf("invalid OCI descriptor")
		}
		if previous, ok := visited[d.Digest]; ok {
			if previous.Size != d.Size {
				return fmt.Errorf("conflicting OCI descriptor size")
			}
			return nil
		}
		file := filepath.Join(directory, "blobs", "sha256", strings.TrimPrefix(d.Digest, "sha256:"))
		digest, size, err := FileDigest(file)
		if err != nil {
			return err
		}
		if digest != d.Digest || size != d.Size {
			return fmt.Errorf("OCI blob %s content verification failed", d.Digest)
		}
		visited[d.Digest] = d
		if !manifest {
			return nil
		}
		if d.Size > 16<<20 {
			return fmt.Errorf("OCI manifest exceeds 16 MiB")
		}
		b, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		var m imageManifest
		if err := json.Unmarshal(b, &m); err != nil {
			return err
		}
		if m.SchemaVersion != 2 {
			return fmt.Errorf("unsupported registry manifest schema")
		}
		if len(m.Manifests) > 0 {
			for _, child := range m.Manifests {
				if err := walk(child, true); err != nil {
					return err
				}
			}
			return nil
		}
		if m.Config.Digest == "" {
			return fmt.Errorf("registry manifest lacks its configuration")
		}
		if err := walk(m.Config, false); err != nil {
			return err
		}
		for _, layer := range m.Layers {
			if err := walk(layer, false); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(*top, true); err != nil {
		return nil, err
	}
	var inventory []string
	for _, d := range visited {
		inventory = append(inventory, fmt.Sprintf("%s %d %s", d.Digest, d.Size, d.MediaType))
	}
	sort.Strings(inventory)
	return inventory, nil
}

func (s Service) ProtectImage(ctx context.Context, i Images, ref string) (Dependency, error) {
	parts := strings.Split(ref, "@")
	if len(parts) != 2 || !digestPattern.MatchString(parts[1]) {
		return Dependency{}, fmt.Errorf("image recovery requires repository@sha256 identity")
	}
	r := Requirement{Kind: "image", ID: ref, Digest: parts[1]}
	if d, err := s.Find(ctx, r, time.Now().Add(Retention)); err == nil {
		return d, nil
	}
	dir, err := os.MkdirTemp("", "recovery-oci-*")
	if err != nil {
		return Dependency{}, err
	}
	defer os.RemoveAll(dir)
	if err := i.copy(ctx, "docker://"+ref, "oci:"+dir+":recovery"); err != nil {
		return Dependency{}, err
	}
	inventory, err := InspectOCI(dir, parts[1])
	if err != nil {
		return Dependency{}, err
	}
	f, err := temporary()
	if err != nil {
		return Dependency{}, err
	}
	defer os.Remove(f.Name())
	w := tar.NewWriter(f)
	err = filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("OCI export contains a nonregular file")
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if err := w.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel), Mode: 0600, Size: info.Size()}); err != nil {
			return err
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		defer input.Close()
		_, err = io.Copy(w, input)
		return err
	})
	closeErr := w.Close()
	fileErr := f.Close()
	if err != nil {
		return Dependency{}, err
	}
	if closeErr != nil {
		return Dependency{}, closeErr
	}
	if fileErr != nil {
		return Dependency{}, fileErr
	}
	return s.Protect(ctx, r, f.Name(), false, inventory)
}

func (s Service) verifyImage(ctx context.Context, d Dependency) error {
	if len(d.Objects) != 1 {
		return fmt.Errorf("registry dependency must contain one complete OCI export")
	}
	f, err := temporary()
	if err != nil {
		return err
	}
	f.Close()
	defer os.Remove(f.Name())
	if err := s.Storage.Get(ctx, d.Objects[0], f.Name()); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "verify-oci-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := extractOCI(f.Name(), dir); err != nil {
		return err
	}
	inventory, err := InspectOCI(dir, d.Digest)
	if err != nil {
		return err
	}
	if string(jsonBytes(inventory)) != string(jsonBytes(d.Inventory)) {
		return fmt.Errorf("registry recovery digest inventory mismatch")
	}
	return nil
}

func extractOCI(file, dir string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	reader := tar.NewReader(f)
	for {
		h, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(h.Name)
		if filepath.IsAbs(name) || filepath.Clean(name) != name || strings.HasPrefix(name, "../") || name == ".." || h.Typeflag != tar.TypeReg {
			return fmt.Errorf("unsafe registry recovery archive entry")
		}
		target := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, err = io.Copy(output, reader)
		closeErr := output.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
}

func (s Service) RestoreImage(ctx context.Context, i Images, d Dependency, repository string) error {
	if d.Kind != "image" || len(d.Objects) != 1 || !digestPattern.MatchString(d.Digest) || strings.ContainsAny(repository, "@ \n\r") {
		return fmt.Errorf("invalid image recovery inventory or destination")
	}
	f, err := temporary()
	if err != nil {
		return err
	}
	f.Close()
	defer os.Remove(f.Name())
	if err := s.verifyObject(ctx, d.Objects[0], time.Now(), true); err != nil {
		return err
	}
	if err := s.Storage.Get(ctx, d.Objects[0], f.Name()); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "restore-oci-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := extractOCI(f.Name(), dir); err != nil {
		return err
	}
	inventory, err := InspectOCI(dir, d.Digest)
	if err != nil {
		return err
	}
	if string(jsonBytes(inventory)) != string(jsonBytes(d.Inventory)) {
		return fmt.Errorf("registry recovery digest inventory mismatch")
	}
	// The transport writes a tag, then availability is checked using the original
	// digest. The tag is never retained as a recovery identity or read dependency.
	if err := i.copy(ctx, "oci:"+dir+":recovery", "docker://"+repository+":recovery-"+strings.TrimPrefix(d.Digest, "sha256:")); err != nil {
		return err
	}
	b, err := i.inspect(ctx, repository+"@"+d.Digest)
	if err != nil || Digest(b) != d.Digest {
		return fmt.Errorf("imported registry image is unavailable by its original digest")
	}
	return nil
}
