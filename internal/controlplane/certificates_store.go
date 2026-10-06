package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"ebof-wg-mesh/internal/controlplane/certificates"
	"ebof-wg-mesh/internal/controlplane/secretkeys"
	"ebof-wg-mesh/internal/controlplane/xds"
)

var errCertificateEncryptionUnavailable = errors.New("certificate keys need envelope encryption")

// certificatePersistence keeps ingress certificates. Private keys are envelope
// encrypted under the platform ingress DEK; the database holds only ciphertext.
type certificatePersistence struct {
	*database
	secrets func() *secretkeys.Service
}

func certificateKeyAAD(fingerprint string) []byte {
	return []byte("ingress-certificate/v1\x00" + fingerprint)
}

func acmeAccountKeyAAD(directoryURL string) []byte {
	return []byte("acme-account/v1\x00" + directoryURL)
}

func (s *certificatePersistence) encrypt(ctx context.Context, q secretkeys.Querier, aad, plaintext []byte) (secretkeys.Ciphertext, error) {
	secrets := s.secrets()
	if secrets == nil {
		return secretkeys.Ciphertext{}, errCertificateEncryptionUnavailable
	}
	return secrets.Encrypt(ctx, q, secretkeys.IngressTLSScope, aad, plaintext)
}

func (s *certificatePersistence) decrypt(ctx context.Context, ciphertext secretkeys.Ciphertext, aad []byte) ([]byte, error) {
	secrets := s.secrets()
	if secrets == nil {
		return nil, errCertificateEncryptionUnavailable
	}
	return secrets.Decrypt(ctx, s.db, ciphertext, aad)
}

const certificateRecordColumns = `hostname, fingerprint, not_after, renew_at, attempts, next_attempt_at, last_error`

func scanCertificateRecord(scan func(...any) error) (certificates.Record, error) {
	var record certificates.Record
	var notAfter, renewAt sql.NullTime
	if err := scan(&record.Hostname, &record.Fingerprint, &notAfter, &renewAt, &record.Attempts, &record.NextAttemptAt, &record.LastError); err != nil {
		return certificates.Record{}, err
	}
	record.NotAfter, record.RenewAt = notAfter.Time, renewAt.Time
	return record, nil
}

func (s *certificatePersistence) ListCertificates(ctx context.Context) ([]certificates.Record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+certificateRecordColumns+` FROM ingress_certificates ORDER BY hostname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []certificates.Record
	for rows.Next() {
		record, err := scanCertificateRecord(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

func (s *certificatePersistence) CertificateRecord(ctx context.Context, hostname string) (certificates.Record, bool, error) {
	record, err := scanCertificateRecord(s.db.QueryRowContext(ctx,
		`SELECT `+certificateRecordColumns+` FROM ingress_certificates WHERE hostname = $1`, hostname).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return certificates.Record{}, false, nil
	}
	if err != nil {
		return certificates.Record{}, false, err
	}
	return record, true, nil
}

func (s *certificatePersistence) SaveIssued(ctx context.Context, version certificates.Version, renewAt time.Time) error {
	return s.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		key, err := s.encrypt(ctx, tx, certificateKeyAAD(version.Fingerprint), version.KeyPEM)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `INSERT INTO ingress_certificate_versions(
				fingerprint, hostname, chain_pem, key_dek_id, key_ciphertext, not_before, not_after, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (fingerprint) DO NOTHING`,
			version.Fingerprint, version.Hostname, version.ChainPEM, key.DEKID, key.Data, version.NotBefore, version.NotAfter, now,
		); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO ingress_certificates(
				hostname, fingerprint, not_after, renew_at, attempts, next_attempt_at, last_error, updated_at
			) VALUES ($1, $2, $3, $4, 0, $4, '', $5) ON CONFLICT(hostname) DO UPDATE SET fingerprint=EXCLUDED.fingerprint, not_after=EXCLUDED.not_after, renew_at=EXCLUDED.renew_at, attempts=0, next_attempt_at=EXCLUDED.next_attempt_at, last_error='', updated_at=EXCLUDED.updated_at`,
			version.Hostname, version.Fingerprint, version.NotAfter, renewAt, now,
		)
		return err
	})
}

func (s *certificatePersistence) RecordFailure(ctx context.Context, hostname string, attempts int, nextAttempt time.Time, message string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO ingress_certificates(
			hostname, attempts, next_attempt_at, last_error, updated_at
		) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (hostname) DO UPDATE SET
			attempts = EXCLUDED.attempts, next_attempt_at = EXCLUDED.next_attempt_at,
			last_error = EXCLUDED.last_error, updated_at = EXCLUDED.updated_at`,
		hostname, attempts, nextAttempt, message, time.Now().UTC(),
	)
	return err
}

func (s *certificatePersistence) CertificateVersion(ctx context.Context, fingerprint string) (certificates.Version, error) {
	version := certificates.Version{Fingerprint: fingerprint}
	var key secretkeys.Ciphertext
	if err := s.db.QueryRowContext(ctx, `SELECT hostname, chain_pem, key_dek_id, key_ciphertext, not_before, not_after
		FROM ingress_certificate_versions WHERE fingerprint = $1`, fingerprint).Scan(
		&version.Hostname, &version.ChainPEM, &key.DEKID, &key.Data, &version.NotBefore, &version.NotAfter,
	); err != nil {
		return certificates.Version{}, err
	}
	plaintext, err := s.decrypt(ctx, key, certificateKeyAAD(fingerprint))
	if err != nil {
		return certificates.Version{}, err
	}
	version.KeyPEM = plaintext
	return version, nil
}

func (s *certificatePersistence) PruneCertificates(ctx context.Context, keep []string, retainAfter time.Time) error {
	keep = slices.Clone(keep)
	if keep == nil {
		keep = []string{}
	}
	return s.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM ingress_certificates c
			WHERE NOT (c.hostname = ANY($1::TEXT[]))
			  AND NOT EXISTS (SELECT 1 FROM domain_bindings d WHERE d.hostname = c.hostname)`, keep); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM ingress_certificate_versions v
			WHERE v.created_at < $1
			  AND NOT EXISTS (SELECT 1 FROM ingress_certificates c WHERE c.fingerprint = v.fingerprint)`, retainAfter); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM acme_http_challenges WHERE expires_at <= statement_timestamp()`)
		return err
	})
}

func (s *certificatePersistence) PutChallenge(ctx context.Context, challenge xds.Challenge, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO acme_http_challenges(token, hostname, key_authorization, expires_at)
		VALUES ($1, $2, $3, $4) ON CONFLICT(token) DO UPDATE SET hostname=EXCLUDED.hostname,key_authorization=EXCLUDED.key_authorization,expires_at=EXCLUDED.expires_at`, challenge.Token, challenge.Hostname, challenge.KeyAuthorization, expiresAt)
	return err
}

func (s *certificatePersistence) DeleteChallenge(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM acme_http_challenges WHERE token = $1`, token)
	return err
}

func (s *certificatePersistence) ListChallenges(ctx context.Context) ([]xds.Challenge, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT hostname, token, key_authorization FROM acme_http_challenges
		WHERE expires_at > statement_timestamp() ORDER BY hostname, token`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []xds.Challenge
	for rows.Next() {
		var challenge xds.Challenge
		if err := rows.Scan(&challenge.Hostname, &challenge.Token, &challenge.KeyAuthorization); err != nil {
			return nil, err
		}
		out = append(out, challenge)
	}
	return out, rows.Err()
}

func (s *certificatePersistence) LoadACMEAccount(ctx context.Context, directoryURL string) (certificates.Account, bool, error) {
	account := certificates.Account{DirectoryURL: directoryURL}
	var key secretkeys.Ciphertext
	err := s.db.QueryRowContext(ctx, `SELECT account_uri, key_dek_id, key_ciphertext FROM acme_accounts WHERE directory_url = $1`,
		directoryURL).Scan(&account.URI, &key.DEKID, &key.Data)
	if errors.Is(err, sql.ErrNoRows) {
		return certificates.Account{}, false, nil
	}
	if err != nil {
		return certificates.Account{}, false, err
	}
	plaintext, err := s.decrypt(ctx, key, acmeAccountKeyAAD(directoryURL))
	if err != nil {
		return certificates.Account{}, false, err
	}
	account.KeyPEM = plaintext
	return account, true, nil
}

func (s *certificatePersistence) SaveACMEAccount(ctx context.Context, account certificates.Account) error {
	return s.withCoordinationTx(ctx, func(ctx context.Context, tx *sql.Tx) error {
		key, err := s.encrypt(ctx, tx, acmeAccountKeyAAD(account.DirectoryURL), account.KeyPEM)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO acme_accounts(directory_url, account_uri, key_dek_id, key_ciphertext, created_at)
			VALUES ($1, $2, $3, $4, $5) ON CONFLICT(directory_url) DO UPDATE SET account_uri=EXCLUDED.account_uri,key_dek_id=EXCLUDED.key_dek_id,key_ciphertext=EXCLUDED.key_ciphertext,created_at=EXCLUDED.created_at`, account.DirectoryURL, account.URI, key.DEKID, key.Data, time.Now().UTC())
		return err
	})
}

// RoutedHostnames lists the live domain bindings and the operator's static
// hosts. A custom binding carries its service's generated hostname, which its
// CNAME must point at. Only the live owner holds an authoritative projection.
func (s *routingPersistence) RoutedHostnames(context.Context) ([]certificates.Hostname, error) {
	live := s.live
	if live == nil || !live.Publishing() {
		return nil, nil
	}
	domains := live.Product().DurableState.Domains
	generated := map[string]string{}
	for _, domain := range domains {
		if domain.PlatformGenerated {
			generated[domain.ServiceID] = domain.Hostname
		}
	}
	seen := map[string]struct{}{}
	var out []certificates.Hostname
	add := func(host certificates.Hostname) {
		host.Name = strings.ToLower(strings.TrimSpace(host.Name))
		if host.Name == "" {
			return
		}
		if _, ok := seen[host.Name]; ok {
			return
		}
		seen[host.Name] = struct{}{}
		out = append(out, host)
	}
	for _, domain := range domains {
		host := certificates.Hostname{Name: domain.Hostname, PlatformGenerated: domain.PlatformGenerated}
		if !domain.PlatformGenerated {
			host.PlatformHostname = generated[domain.ServiceID]
		}
		add(host)
	}
	for _, name := range s.staticHosts {
		add(certificates.Hostname{Name: name})
	}
	slices.SortFunc(out, func(a, b certificates.Hostname) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// ingressInputs joins the routed backends with the certificates that the
// snapshot serves.
type ingressInputs struct {
	routing      *routingPersistence
	certificates *certificates.Service
}

func (s ingressInputs) IngressInputs(ctx context.Context) (xds.Inputs, error) {
	backends, err := s.routing.HealthyIngressBackends(ctx)
	if err != nil {
		return xds.Inputs{}, err
	}
	certs, challenges, err := s.certificates.IngressCertificates(ctx)
	if err != nil {
		return xds.Inputs{}, err
	}
	return xds.Inputs{Backends: backends, Certificates: certs, Challenges: challenges}, nil
}

func (s ingressInputs) WithLeaseGuard(ctx context.Context, fn func() error) error {
	return s.routing.WithLeaseGuard(ctx, fn)
}
