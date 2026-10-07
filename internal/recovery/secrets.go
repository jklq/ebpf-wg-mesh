package recovery

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"ebof-wg-mesh/internal/controlplane/secretkeys"
	"ebof-wg-mesh/internal/controlplane/signkeys"
)

type WrapProbe struct {
	Version    string `json:"version"`
	Purpose    string `json:"purpose"`
	Ciphertext []byte `json:"ciphertext"`
	Size       int    `json:"size"`
}
type ConsoleProbe struct {
	User       string `json:"user"`
	Subject    string `json:"subject"`
	Kind       string `json:"kind"`
	Ciphertext string `json:"ciphertext"`
}

func sameSecret(r Requirement, a, b []byte) bool {
	if r.Kind != "keyring" {
		return bytes.Equal(a, b)
	}
	var left, right struct {
		Keys map[string]struct {
			Key       string `json:"key"`
			Algorithm string `json:"algorithm"`
		} `json:"keys"`
	}
	if json.Unmarshal(a, &left) != nil || json.Unmarshal(b, &right) != nil {
		return false
	}
	l, lok := left.Keys[r.ID]
	rr, rok := right.Keys[r.ID]
	return lok && rok && l.Key != "" && l.Key == rr.Key && l.Algorithm == rr.Algorithm
}

func readSecretProbes(ctx context.Context, tx *sql.Tx, s *Snapshot, console string) error {
	for _, query := range []struct {
		SQL     string
		Purpose func(string) string
		Size    int
	}{
		{`SELECT k.provider_ref,d.id,d.wrapped_dek FROM envelope_data_keys d JOIN envelope_keys k ON k.id=d.wrapping_key_id`, secretkeys.DEKWrapPurpose, 32},
		{`SELECT k.provider_ref,d.id,d.wrapped_key FROM platform_signing_keys d JOIN envelope_keys k ON k.id=d.wrapping_key_id`, signkeys.SignKeyWrapPurpose, 0},
	} {
		rows, err := tx.QueryContext(ctx, query.SQL)
		if err != nil {
			return err
		}
		for rows.Next() {
			var p WrapProbe
			var id string
			if err := rows.Scan(&p.Version, &id, &p.Ciphertext); err != nil {
				rows.Close()
				return err
			}
			p.Purpose = query.Purpose(id)
			p.Size = query.Size
			s.WrapProbes = append(s.WrapProbes, p)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT user_id,provider_subject,access_token,refresh_token FROM `+console+`.accounts WHERE provider='github'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var user, subject, access, refresh string
		if err := rows.Scan(&user, &subject, &access, &refresh); err != nil {
			return err
		}
		for kind, token := range map[string]string{"access": access, "refresh": refresh} {
			if token != "" {
				s.ConsoleProbes = append(s.ConsoleProbes, ConsoleProbe{user, subject, kind, token})
			}
		}
	}
	return rows.Err()
}

func (s Service) bundle(ctx context.Context, d Dependency) ([]byte, error) {
	if len(d.Objects) != 1 {
		return nil, fmt.Errorf("invalid encrypted secret inventory")
	}
	f, err := temporary()
	if err != nil {
		return nil, err
	}
	f.Close()
	defer os.Remove(f.Name())
	if err := s.Storage.Get(ctx, d.Objects[0], f.Name()); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		return nil, err
	}
	if Digest(b) != d.Objects[0].Digest {
		return nil, fmt.Errorf("secret bundle content digest mismatch")
	}
	return s.open(d.Kind, d.ID, b)
}

// Verify the protected material against ciphertext read at the point's timestamp.
// A correctly shaped key with the right name but different bytes cannot pass.
func (s Service) verifyDecryption(ctx context.Context, p Point) error {
	ring := struct {
		Version int                        `json:"version"`
		Keys    map[string]json.RawMessage `json:"keys"`
	}{1, map[string]json.RawMessage{}}
	var consoleKeys [][]byte
	for _, d := range p.Dependencies {
		if d.Kind != "keyring" && d.Kind != "console-key" {
			continue
		}
		plain, err := s.bundle(ctx, d)
		if err != nil {
			return err
		}
		if d.Kind == "keyring" {
			var keys struct {
				Keys map[string]json.RawMessage `json:"keys"`
			}
			err = json.Unmarshal(plain, &keys)
			if err == nil {
				entry, ok := keys.Keys[d.ID]
				if !ok {
					err = fmt.Errorf("protected keyring version is missing")
				} else {
					var material struct {
						Key string `json:"key"`
					}
					if err := json.Unmarshal(entry, &material); err != nil {
						clear(plain)
						return err
					}
					key, err := base64.StdEncoding.DecodeString(material.Key)
					same := err == nil && bytes.Equal(key, s.RecoveryKey)
					clear(key)
					if same {
						clear(plain)
						return fmt.Errorf("recovery encryption key duplicates a protected platform master key")
					}
					ring.Keys[d.ID] = entry
				}
			}
		} else {
			var key []byte
			key, err = base64.StdEncoding.DecodeString(strings.TrimSpace(string(plain)))
			if err == nil && len(key) == 32 {
				consoleKeys = append(consoleKeys, key)
				defer clear(key)
			} else {
				err = fmt.Errorf("invalid protected console key")
			}
		}
		clear(plain)
		if err != nil {
			return err
		}
	}
	f, err := temporary()
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := json.NewEncoder(f).Encode(ring); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	provider, err := secretkeys.NewKeyring(f.Name(), secretkeys.KeyringOptions{})
	if err != nil {
		return err
	}
	for _, probe := range p.Snapshot.WrapProbes {
		plain, err := provider.Unwrap(ctx, probe.Version, probe.Purpose, probe.Ciphertext)
		if err != nil {
			return fmt.Errorf("protected key cannot decrypt historical ciphertext for %s: %w", probe.Version, err)
		}
		valid := len(plain) > 0 && (probe.Size == 0 || len(plain) == probe.Size)
		clear(plain)
		if !valid {
			return fmt.Errorf("historical wrapped key has invalid material")
		}
	}
	for _, probe := range p.Snapshot.ConsoleProbes {
		if !strings.HasPrefix(probe.Ciphertext, "ghe1.") {
			return fmt.Errorf("historical console token has invalid encryption format")
		}
		b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(probe.Ciphertext, "ghe1."))
		if err != nil || len(b) <= 28 {
			return fmt.Errorf("historical console token is invalid")
		}
		opened := false
		for _, key := range consoleKeys {
			block, _ := aes.NewCipher(key)
			aead, _ := cipher.NewGCM(block)
			plain, err := aead.Open(nil, b[:12], b[12:], []byte("github-oauth-token:v1\x00"+probe.User+"\x00"+probe.Subject+"\x00"+probe.Kind))
			if err == nil {
				clear(plain)
				opened = true
				break
			}
		}
		if !opened {
			return fmt.Errorf("protected keys cannot decrypt historical console tokens")
		}
	}
	return nil
}
