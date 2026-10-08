package recovery

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Service struct {
	Storage     Storage
	Prefix      string
	RecoveryKey []byte
}

func FileDigest(file string) (string, int64, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return fmt.Sprintf("sha256:%x", h.Sum(nil)), n, err
}

func temporary() (*os.File, error) { return os.CreateTemp("", "platform-recovery-*") }

func (s Service) seal(kind, id string, plain []byte) ([]byte, error) {
	if len(s.RecoveryKey) != 32 {
		return nil, fmt.Errorf("independent recovery key must contain 32 raw bytes")
	}
	block, err := aes.NewCipher(s.RecoveryKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plain, []byte("platform-recovery/v1/"+kind+"/"+id)), nil
}
func (s Service) open(kind, id string, b []byte) ([]byte, error) {
	if len(s.RecoveryKey) != 32 {
		return nil, fmt.Errorf("independent recovery key is unavailable")
	}
	block, _ := aes.NewCipher(s.RecoveryKey)
	aead, _ := cipher.NewGCM(block)
	if len(b) < aead.NonceSize() {
		return nil, fmt.Errorf("truncated recovery secret bundle")
	}
	plain, err := aead.Open(nil, b[:aead.NonceSize()], b[aead.NonceSize():], []byte("platform-recovery/v1/"+kind+"/"+id))
	if err != nil {
		return nil, fmt.Errorf("recovery secret bundle authentication failed")
	}
	return plain, nil
}

// Protect records a receipt only after a content-checked, protected copy exists.
// Secrets are encrypted before upload; the platform keyring is never the cipher.
func (s Service) Protect(ctx context.Context, r Requirement, file string, secret bool, inventory []string) (Dependency, error) {
	digest, _, err := FileDigest(file)
	if err != nil {
		return Dependency{}, err
	}
	if r.Digest != "" && digest != r.Digest && r.Kind != "image" {
		return Dependency{}, fmt.Errorf("%s content digest differs from its immutable identity", identity(r))
	}
	until := time.Now().UTC().Add(Retention + 24*time.Hour)
	if d, err := s.Find(ctx, r, until); err == nil && len(d.Objects) == 1 {
		if !secret && d.Objects[0].Digest == digest {
			return d, nil
		}
		if secret {
			protected, err := s.bundle(ctx, d)
			if err != nil {
				return Dependency{}, err
			}
			defer clear(protected)
			local, err := os.ReadFile(file)
			if err != nil {
				return Dependency{}, err
			}
			defer clear(local)
			if sameSecret(r, protected, local) {
				return d, nil
			}
			return Dependency{}, fmt.Errorf("secret version %s already protects different material; provision a new version", identity(r))
		}
	}
	if secret {
		plain, err := os.ReadFile(file)
		if err != nil {
			return Dependency{}, err
		}
		defer clear(plain)
		b, err := s.seal(r.Kind, r.ID, plain)
		if err != nil {
			return Dependency{}, err
		}
		f, err := temporary()
		if err != nil {
			return Dependency{}, err
		}
		defer os.Remove(f.Name())
		if _, err := f.Write(b); err != nil {
			f.Close()
			return Dependency{}, err
		}
		if err := f.Close(); err != nil {
			return Dependency{}, err
		}
		file = f.Name()
		digest = Digest(b)
	}
	o, err := s.Storage.Put(ctx, s.Prefix+"/objects/"+strings.TrimPrefix(digest, "sha256:"), file, until)
	if err != nil {
		return Dependency{}, err
	}
	d := Dependency{Kind: r.Kind, ID: r.ID, Digest: r.Digest, Objects: []Object{o}, Inventory: inventory}
	if err := s.verifyObject(ctx, o, until, true); err != nil {
		return Dependency{}, err
	}
	if _, err := s.putJSON(ctx, s.receiptPrefix(r)+strings.TrimPrefix(Digest(jsonBytes(d)), "sha256:"), d, until); err != nil {
		return Dependency{}, err
	}
	return d, nil
}

func (s Service) receiptPrefix(r Requirement) string {
	return s.Prefix + "/receipts/" + strings.TrimPrefix(Digest([]byte(identity(r)+"/"+r.Digest)), "sha256:") + "/"
}
func (s Service) putJSON(ctx context.Context, key string, v any, until time.Time) (Object, error) {
	f, err := temporary()
	if err != nil {
		return Object{}, err
	}
	defer os.Remove(f.Name())
	if err := json.NewEncoder(f).Encode(v); err != nil {
		f.Close()
		return Object{}, err
	}
	if err := f.Close(); err != nil {
		return Object{}, err
	}
	o, err := s.Storage.Put(ctx, key, f.Name(), until)
	if err != nil {
		return Object{}, err
	}
	return o, s.verifyObject(ctx, o, until, true)
}

func (s Service) readJSON(ctx context.Context, o Object, v any) error {
	f, err := temporary()
	if err != nil {
		return err
	}
	f.Close()
	defer os.Remove(f.Name())
	if err := s.Storage.Get(ctx, o, f.Name()); err != nil {
		return err
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		return err
	}
	if o.Digest != "" && Digest(b) != o.Digest {
		return fmt.Errorf("protected catalog content digest mismatch")
	}
	return json.Unmarshal(b, v)
}

func (s Service) Find(ctx context.Context, r Requirement, until time.Time) (Dependency, error) {
	versions, err := s.Storage.Versions(ctx, s.receiptPrefix(r))
	if err != nil {
		return Dependency{}, err
	}
	var last error
	for _, o := range versions {
		catalog, err := s.Storage.Inspect(ctx, o)
		if err != nil {
			last = err
			continue
		}
		o.Digest = catalog.Digest
		var d Dependency
		if err := s.readJSON(ctx, o, &d); err != nil {
			last = err
			continue
		}
		if d.Kind != r.Kind || d.ID != r.ID || d.Digest != r.Digest || len(d.Objects) == 0 {
			continue
		}
		good := true
		for n, object := range d.Objects {
			current, err := s.Storage.Inspect(ctx, object)
			if err != nil {
				last = err
				good = false
				break
			}
			if current.RetainUntil.Before(until) {
				if err = s.Storage.Retain(ctx, object, until); err != nil {
					last = err
					good = false
					break
				}
				current, err = s.Storage.Inspect(ctx, object)
				if err != nil {
					last = err
					good = false
					break
				}
			}
			if current.Size != object.Size || current.Digest != "" && current.Digest != object.Digest || current.RetainUntil.Before(until) {
				last = fmt.Errorf("protected object content or retention differs")
				good = false
				break
			}
			d.Objects[n].RetainUntil = current.RetainUntil
		}
		if good {
			if catalog.RetainUntil.Before(until) {
				if err := s.Storage.Retain(ctx, o, until); err != nil {
					last = err
					continue
				}
				retained, err := s.Storage.Inspect(ctx, o)
				if err != nil || retained.RetainUntil.Before(until) {
					last = fmt.Errorf("receipt retention unresolved: %v", err)
					continue
				}
			}
			return d, nil
		}
	}
	return Dependency{}, fmt.Errorf("missing protected %s: %v", identity(r), last)
}

func (s Service) verifyObject(ctx context.Context, o Object, until time.Time, content bool) error {
	if o.Key == "" || o.Version == "" || o.Version == "null" || !digestPattern.MatchString(o.Digest) || o.Size < 0 {
		return fmt.Errorf("invalid protected object inventory")
	}
	current, err := s.Storage.Inspect(ctx, o)
	if err != nil {
		return err
	}
	if current.Size != o.Size || (current.Digest != "" && current.Digest != o.Digest) || current.RetainUntil.Before(until) {
		return fmt.Errorf("object %s version %s has mismatched content or insufficient retention", o.Key, o.Version)
	}
	if content {
		f, err := temporary()
		if err != nil {
			return err
		}
		f.Close()
		defer os.Remove(f.Name())
		if err := s.Storage.Get(ctx, o, f.Name()); err != nil {
			return err
		}
		digest, size, err := FileDigest(f.Name())
		if err != nil {
			return err
		}
		if digest != o.Digest || size != o.Size {
			return fmt.Errorf("protected object %s failed content verification", o.Key)
		}
	}
	return nil
}

func (s Service) Verify(ctx context.Context, p Point, content bool) Report {
	r := Report{Timestamp: p.Snapshot.Timestamp}
	if err := p.Validate(time.Now().UTC()); err != nil {
		r.Failures = append(r.Failures, err.Error())
		return r
	}
	if err := s.Storage.Check(ctx); err != nil {
		r.Failures = append(r.Failures, err.Error())
		return r
	}
	dependencies := map[string]Dependency{}
	for _, d := range p.Dependencies {
		k := d.Kind + "/" + d.ID
		if _, ok := dependencies[k]; ok {
			r.Failures = append(r.Failures, "duplicate dependency "+k)
		}
		dependencies[k] = d
	}
	for _, need := range p.Requirements() {
		d, ok := dependencies[identity(need)]
		if !ok || d.Digest != need.Digest || len(d.Objects) == 0 {
			r.Missing = append(r.Missing, identity(need))
			continue
		}
		if need.Kind == "image" && len(d.Inventory) == 0 {
			r.Missing = append(r.Missing, "complete digest inventory for "+need.ID)
		}
		for _, o := range d.Objects {
			if err := s.verifyObject(ctx, o, p.ExpiresAt, content); err != nil {
				r.Failures = append(r.Failures, identity(need)+": "+err.Error())
			}
		}
		if content && (d.Kind == "keyring" || d.Kind == "console-key" || d.Kind == "external-secret") {
			if err := s.verifySecret(ctx, d); err != nil {
				r.Failures = append(r.Failures, identity(need)+": "+err.Error())
			}
		}
		if content && d.Kind == "image" {
			if err := s.verifyImage(ctx, d); err != nil {
				r.Failures = append(r.Failures, identity(need)+": "+err.Error())
			}
		}
	}
	for _, o := range p.Database.Objects {
		if err := s.verifyObject(ctx, o, p.ExpiresAt, content); err != nil {
			r.Failures = append(r.Failures, "database: "+err.Error())
		}
	}
	if content && len(r.Missing) == 0 {
		if err := s.verifyDecryption(ctx, p); err != nil {
			r.Failures = append(r.Failures, err.Error())
		}
	}
	sort.Strings(r.Missing)
	r.Complete = len(r.Missing) == 0 && len(r.Failures) == 0
	return r
}

func (s Service) verifySecret(ctx context.Context, d Dependency) error {
	if len(d.Objects) != 1 {
		return fmt.Errorf("secret bundle must have one authenticated object")
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
	b, err := os.ReadFile(f.Name())
	if err != nil {
		return err
	}
	plain, err := s.open(d.Kind, d.ID, b)
	if err != nil {
		return err
	}
	defer clear(plain)
	if d.Kind == "keyring" {
		var ring struct {
			Version int `json:"version"`
			Keys    map[string]struct {
				Algorithm string `json:"algorithm"`
				Key       string `json:"key"`
			} `json:"keys"`
		}
		if err := json.Unmarshal(plain, &ring); err != nil {
			return fmt.Errorf("invalid protected keyring")
		}
		entry, ok := ring.Keys[d.ID]
		material, err := base64.StdEncoding.DecodeString(entry.Key)
		defer clear(material)
		if !ok || ring.Version != 1 || entry.Algorithm != "AES-256-GCM" || err != nil || len(material) != 32 {
			return fmt.Errorf("protected keyring lacks required version %s", d.ID)
		}
	}
	if d.Kind == "console-key" {
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(plain)))
		defer clear(key)
		if err != nil || len(key) != 32 {
			return fmt.Errorf("protected console encryption key is invalid")
		}
	}
	if len(plain) == 0 {
		return fmt.Errorf("protected secret bundle is empty")
	}
	return nil
}

// RequireSecret compares the protected, authenticated bundle to local material.
// A receipt or a key version name alone is insufficient to permit activation.
func (s Service) RequireSecret(ctx context.Context, r Requirement, file string) error {
	if err := s.Storage.Check(ctx); err != nil {
		return err
	}
	d, err := s.Find(ctx, r, time.Now().Add(Retention))
	if err != nil {
		return err
	}
	if err := s.verifySecret(ctx, d); err != nil {
		return err
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
	b, err := os.ReadFile(f.Name())
	if err != nil {
		return err
	}
	plain, err := s.open(r.Kind, r.ID, b)
	if err != nil {
		return err
	}
	defer clear(plain)
	local, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	defer clear(local)
	if !sameSecret(r, plain, local) {
		return fmt.Errorf("protected %s does not match local secret material", identity(r))
	}
	return nil
}

// Collect deletes only unlocked versions unreferenced by every retained point.
// Extending Object Lock before publication makes concurrent publication safe:
// a raced delete either wins first (publication fails) or S3 refuses deletion.
func (s Service) Collect(ctx context.Context) (int, error) {
	points, err := s.Points(ctx, "")
	if err != nil {
		return 0, err
	}
	used := map[string]bool{}
	for _, p := range points {
		for _, o := range p.Database.Objects {
			used[o.Key+"\x00"+o.Version] = true
		}
		for _, d := range p.Dependencies {
			for _, o := range d.Objects {
				used[o.Key+"\x00"+o.Version] = true
			}
		}
	}
	versions, err := s.Storage.Versions(ctx, s.Prefix+"/")
	if err != nil {
		return 0, err
	}
	count := 0
	for _, o := range versions {
		if used[o.Key+"\x00"+o.Version] {
			continue
		}
		current, err := s.Storage.Inspect(ctx, o)
		if err != nil {
			return count, err
		}
		if !current.RetainUntil.Before(time.Now()) {
			continue
		}
		if err := s.Storage.Delete(ctx, o); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func privateFile(file string) ([]byte, error) {
	info, err := os.Stat(file)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(file) || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("recovery key/configuration must be a private absolute regular file")
	}
	return os.ReadFile(file)
}
