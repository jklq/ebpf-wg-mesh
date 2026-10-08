package recovery

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"ebof-wg-mesh/internal/controlplane/secretkeys"
)

type memoryStorage struct {
	objects    map[string]Object
	data       map[string][]byte
	sequence   int
	failRetain bool
}

func newStorage() *memoryStorage {
	return &memoryStorage{objects: map[string]Object{}, data: map[string][]byte{}}
}
func objectID(o Object) string                       { return o.Key + "\x00" + o.Version }
func (m *memoryStorage) Check(context.Context) error { return nil }
func (m *memoryStorage) Put(_ context.Context, key, file string, until time.Time) (Object, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return Object{}, err
	}
	m.sequence++
	o := Object{Key: key, Version: fmt.Sprintf("%08d", m.sequence), Digest: Digest(b), Size: int64(len(b)), RetainUntil: until}
	m.objects[objectID(o)] = o
	m.data[objectID(o)] = b
	return o, nil
}
func (m *memoryStorage) Get(_ context.Context, o Object, file string) error {
	b, ok := m.data[objectID(o)]
	if !ok {
		return fmt.Errorf("missing version")
	}
	return os.WriteFile(file, b, 0600)
}
func (m *memoryStorage) Inspect(_ context.Context, o Object) (Object, error) {
	found, ok := m.objects[objectID(o)]
	if !ok {
		return Object{}, fmt.Errorf("missing version")
	}
	return found, nil
}
func (m *memoryStorage) Retain(_ context.Context, o Object, t time.Time) error {
	if m.failRetain {
		return fmt.Errorf("retention denied")
	}
	found, ok := m.objects[objectID(o)]
	if !ok {
		return fmt.Errorf("missing version")
	}
	if t.After(found.RetainUntil) {
		found.RetainUntil = t
		m.objects[objectID(o)] = found
	}
	return nil
}
func (m *memoryStorage) Versions(_ context.Context, prefix string) ([]Object, error) {
	var out []Object
	for _, o := range m.objects {
		if strings.HasPrefix(o.Key, prefix) {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version > out[j].Version })
	return out, nil
}
func (m *memoryStorage) Delete(_ context.Context, o Object) error {
	if current := m.objects[objectID(o)]; current.RetainUntil.After(time.Now()) {
		return fmt.Errorf("Object Lock refused delete")
	}
	delete(m.objects, objectID(o))
	delete(m.data, objectID(o))
	return nil
}

func write(t *testing.T, b []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func fixture(t *testing.T, timestamp time.Time) (Service, *memoryStorage, Point) {
	t.Helper()
	m := newStorage()
	s := Service{Storage: m, Prefix: "recovery", RecoveryKey: []byte(strings.Repeat("r", 32))}
	p := Point{Version: 1, Installation: "production", Snapshot: Snapshot{Timestamp: timestamp, Schema: 42, ConsoleSchema: 3, Identities: map[string][]string{"agent_registrations": {}, "ingress_nodes": {}, "platform_signing_keys": {}}}, Database: Database{Collection: "external://recovery", Subdirectory: "full", Layers: []Layer{{End: timestamp}}}, ExpiresAt: timestamp.Add(Retention)}
	o, err := m.Put(context.Background(), "recovery/database/full/data.sst", write(t, []byte("database")), timestamp.Add(Retention))
	if err != nil {
		t.Fatal(err)
	}
	p.Database.Objects = []Object{o}
	for _, kind := range []string{"keyring", "console-key", "external-secret", "installation", "deployment-state", "release", "tool"} {
		r := Requirement{Kind: kind, ID: "required"}
		p.Snapshot.Requirements = append(p.Snapshot.Requirements, r)
		b := []byte("content for " + kind)
		secret := false
		if kind == "keyring" {
			b = jsonBytes(map[string]any{"version": 1, "keys": map[string]any{"required": map[string]any{"algorithm": "AES-256-GCM", "key": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))}}})
			secret = true
		}
		if kind == "console-key" {
			b = []byte(base64.StdEncoding.EncodeToString([]byte(strings.Repeat("c", 32))))
			secret = true
		}
		if kind == "external-secret" {
			secret = true
		}
		if _, err := s.Protect(context.Background(), r, write(t, b), secret, nil); err != nil {
			t.Fatal(err)
		}
	}
	p.Installer = append([]Requirement{}, p.Snapshot.Requirements...)
	return s, m, p
}

func TestDatabaseBackupCannotPublishIncompleteRecoveryPoint(t *testing.T) {
	s, m, p := fixture(t, time.Now().Add(-time.Minute))
	missing := p.Snapshot.Requirements[0]
	receipts, _ := m.Versions(context.Background(), s.receiptPrefix(missing))
	for _, o := range receipts {
		delete(m.objects, objectID(o))
		delete(m.data, objectID(o))
	}
	_, report, err := s.Publish(context.Background(), p)
	if err == nil || len(report.Missing) != 1 {
		t.Fatal("database-only backup became complete", report, err)
	}
	points, _ := s.Points(context.Background(), "production")
	if len(points) != 0 {
		t.Fatal("incomplete point appeared in complete catalog")
	}
}

func TestProtectedPointSurvivesActiveDeletionAndDetectsMissingVersions(t *testing.T) {
	s, m, p := fixture(t, time.Now().Add(-time.Minute))
	object, report, err := s.Publish(context.Background(), p)
	if err != nil || !report.Complete {
		t.Fatal(report, err)
	}
	p, err = s.ReadPoint(context.Background(), object)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.ValidateDependencies(); err != nil {
		t.Fatal(err)
	}
	if report, err := s.CheckFreshness(context.Background(), "production"); err != nil || !report.Complete {
		t.Fatal(report, err)
	}
	// The point works without any active file or database; removing one protected
	// version makes verification fail even though every other dependency survives.
	image := p.Dependencies[0].Objects[0]
	delete(m.objects, objectID(image))
	delete(m.data, objectID(image))
	if report := s.Verify(context.Background(), p, true); report.Complete || len(report.Failures) == 0 {
		t.Fatal("missing protected version did not fail verification", report)
	}
}

func TestSecretCopiesUseIndependentAuthenticatedEncryption(t *testing.T) {
	m := newStorage()
	s := Service{Storage: m, Prefix: "recovery", RecoveryKey: []byte(strings.Repeat("r", 32))}
	r := Requirement{Kind: "external-secret", ID: "provider-v1"}
	file := write(t, []byte("secret credential"))
	d, err := s.Protect(context.Background(), r, file, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw := m.data[objectID(d.Objects[0])]
	if strings.Contains(string(raw), "secret credential") {
		t.Fatal("plaintext uploaded")
	}
	if err := s.RequireSecret(context.Background(), r, file); err != nil {
		t.Fatal(err)
	}
	s.RecoveryKey = []byte(strings.Repeat("x", 32))
	if err := s.RequireSecret(context.Background(), r, file); err == nil {
		t.Fatal("wrong recovery key accepted")
	}
}

func TestInvalidProtectedKeyVersionPreventsCompletion(t *testing.T) {
	s, _, p := fixture(t, time.Now().Add(-time.Minute))
	r := p.Snapshot.Requirements[0]
	m := s.Storage.(*memoryStorage)
	receipts, _ := m.Versions(context.Background(), s.receiptPrefix(r))
	for _, o := range receipts {
		delete(m.objects, objectID(o))
		delete(m.data, objectID(o))
	}
	if _, err := s.Protect(context.Background(), r, write(t, []byte(`{"version":1,"keys":{}}`)), true, nil); err != nil {
		t.Fatal(err)
	}
	if _, report, err := s.Publish(context.Background(), p); err == nil || report.Complete {
		t.Fatal("key name without its material produced a complete point")
	}
}

func TestRetentionFailureAndOldCompletePointDoNotAdvanceObjective(t *testing.T) {
	s, m, p := fixture(t, time.Now().Add(-20*time.Minute))
	m.failRetain = true
	if _, _, err := s.Publish(context.Background(), p); err == nil {
		t.Fatal("retention failure accepted")
	}
	m.failRetain = false
	if _, _, err := s.Publish(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	report, err := s.CheckFreshness(context.Background(), "production")
	if err == nil || !report.Complete {
		t.Fatal("completion time advanced database timestamp", report, err)
	}
}

func TestDependencyRetentionPreventsCollectionOfReferencedObjects(t *testing.T) {
	s, m, p := fixture(t, time.Now().Add(-time.Minute))
	object, _, err := s.Publish(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	p, err = s.ReadPoint(context.Background(), object)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a broken retention backend: dependency reachability still guards
	// collection, and the verifier independently reports the retention failure.
	used := p.Dependencies[0].Objects[0]
	current := m.objects[objectID(used)]
	current.RetainUntil = time.Now().Add(-time.Hour)
	m.objects[objectID(used)] = current
	unused, err := m.Put(context.Background(), "recovery/objects/unreferenced", write(t, []byte("unused")), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.Collect(context.Background())
	if err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if _, ok := m.objects[objectID(used)]; !ok {
		t.Fatal("retained dependency collected")
	}
	if _, ok := m.objects[objectID(unused)]; ok {
		t.Fatal("unreferenced unlocked version leaked")
	}
}

func TestDatabaseChainRejectsGapsAndExcessIncrementals(t *testing.T) {
	_, _, p := fixture(t, time.Now().Add(-time.Minute))
	p.Database.Layers = append(p.Database.Layers, Layer{Start: p.Snapshot.Timestamp.Add(time.Second), End: p.Snapshot.Timestamp.Add(time.Minute)})
	if err := p.Validate(time.Now()); err == nil {
		t.Fatal("backup gap accepted")
	}
	p.Database.Layers = []Layer{{End: p.Snapshot.Timestamp.Add(-50 * time.Minute)}}
	for n := 0; n < 49; n++ {
		start := p.Database.Layers[len(p.Database.Layers)-1].End
		p.Database.Layers = append(p.Database.Layers, Layer{Start: start, End: start.Add(time.Minute)})
	}
	if err := p.Validate(time.Now()); err == nil {
		t.Fatal("unsupported incremental chain accepted")
	}
}

func TestProtectedKeysMustDecryptHistoricalCiphertext(t *testing.T) {
	s, _, p := fixture(t, time.Now().Add(-time.Minute))
	ring := write(t, jsonBytes(map[string]any{"version": 1, "keys": map[string]any{"required": map[string]any{"algorithm": "AES-256-GCM", "key": base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))}}}))
	provider, err := secretkeys.NewKeyring(ring, secretkeys.KeyringOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := provider.Wrap(context.Background(), "required", "historical-dek", []byte(strings.Repeat("d", 32)))
	if err != nil {
		t.Fatal(err)
	}
	p.Snapshot.WrapProbes = []WrapProbe{{Version: "required", Purpose: "historical-dek", Ciphertext: wrapped, Size: 32}}
	block, _ := aes.NewCipher([]byte(strings.Repeat("c", 32)))
	aead, _ := cipher.NewGCM(block)
	nonce := make([]byte, aead.NonceSize())
	token := aead.Seal(nonce, nonce, []byte("token"), []byte("github-oauth-token:v1\x00user\x00subject\x00access"))
	p.Snapshot.ConsoleProbes = []ConsoleProbe{{User: "user", Subject: "subject", Kind: "access", Ciphertext: "ghe1." + base64.RawURLEncoding.EncodeToString(token)}}
	if _, report, err := s.Publish(context.Background(), p); err != nil || !report.Complete {
		t.Fatal(report, err)
	}
	p.Snapshot.WrapProbes[0].Ciphertext[0] ^= 1
	if _, report, err := s.Publish(context.Background(), p); err == nil || report.Complete {
		t.Fatal("unusable historical decryption accepted", report)
	}
}

func TestRecoveryURLPinsCatalogVersionAndCollectIncludesOtherInstallations(t *testing.T) {
	s, m, p := fixture(t, time.Now().Add(-time.Minute))
	p.Installation = "other-installation"
	object, _, err := s.Publish(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := s.ResolvePoint(context.Background(), "recovery-bucket", object.S3URL("recovery-bucket"))
	if err != nil || resolved.Digest != object.Digest {
		t.Fatal(resolved, err)
	}
	for _, invalid := range []string{"s3://recovery-bucket/" + object.Key, object.S3URL("other-bucket"), "s3://recovery-bucket/recovery/objects/blob?versionId=v1"} {
		if _, err := s.ResolvePoint(context.Background(), "recovery-bucket", invalid); err == nil {
			t.Fatal("unversioned or unrelated selection accepted", invalid)
		}
	}
	p, err = s.ReadPoint(context.Background(), object)
	if err != nil {
		t.Fatal(err)
	}
	used := p.Dependencies[0].Objects[0]
	current := m.objects[objectID(used)]
	current.RetainUntil = time.Now().Add(-time.Hour)
	m.objects[objectID(used)] = current
	if _, err := s.Collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.objects[objectID(used)]; !ok {
		t.Fatal("another installation's retained dependency collected")
	}
}
