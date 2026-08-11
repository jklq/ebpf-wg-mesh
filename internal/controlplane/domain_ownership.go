package controlplane

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	domainChallengePrefix      = "_mesh-challenge."
	domainChallengeValuePrefix = "ebpf-wg-mesh-domain-verification="
	domainChallengeTTL         = 30 * time.Minute
)

var (
	errInvalidDomainHostname    = errors.New("hostname must be a valid DNS name")
	errDomainOwnershipNotProven = errors.New("domain ownership TXT record was not found")
)

func canonicalDomainHostname(raw string) (string, error) {
	hostname := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
	if hostname == "" || len(hostname) > 253 || !strings.Contains(hostname, ".") {
		return "", errInvalidDomainHostname
	}
	for _, label := range strings.Split(hostname, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errInvalidDomainHostname
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return "", errInvalidDomainHostname
			}
		}
	}
	return hostname, nil
}

func newDomainChallengeToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate domain ownership challenge: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func domainChallengeRecordName(hostname string) string {
	return domainChallengePrefix + hostname + "."
}

func domainChallengeRecordValue(token string) string {
	return domainChallengeValuePrefix + token
}

func (s *Store) requestDomainOwnershipChallenge(ctx context.Context, subject, projectID, hostname string) (domainOwnershipChallengeRecord, error) {
	if _, err := s.projectByID(ctx, subject, projectID); err != nil {
		return domainOwnershipChallengeRecord{}, err
	}
	token, err := newDomainChallengeToken()
	if err != nil {
		return domainOwnershipChallengeRecord{}, err
	}
	now := time.Now().UTC()
	expiresAt := now.Add(domainChallengeTTL)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO domain_ownership_challenges(hostname, project_id, token, expires_at, created_at)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT(hostname, project_id) DO UPDATE
		 SET token = excluded.token, expires_at = excluded.expires_at, created_at = excluded.created_at`,
		hostname, projectID, token, expiresAt, now,
	); err != nil {
		return domainOwnershipChallengeRecord{}, err
	}
	return domainOwnershipChallengeRecord{
		Hostname: hostname, ProjectID: projectID,
		RecordName:  domainChallengeRecordName(hostname),
		RecordValue: domainChallengeRecordValue(token),
		ExpiresAt:   expiresAt,
	}, nil
}

func (s *Store) domainOwnershipChallenge(ctx context.Context, subject, projectID, hostname string) (domainOwnershipChallengeRecord, error) {
	if _, err := s.projectByID(ctx, subject, projectID); err != nil {
		return domainOwnershipChallengeRecord{}, err
	}
	var token string
	var expiresAt time.Time
	if err := s.db.QueryRowContext(ctx,
		`SELECT token, expires_at
		   FROM domain_ownership_challenges
		  WHERE hostname = $1 AND project_id = $2 AND expires_at > $3`,
		hostname, projectID, time.Now().UTC(),
	).Scan(&token, &expiresAt); err != nil {
		if err == sql.ErrNoRows {
			return domainOwnershipChallengeRecord{}, errDomainOwnershipNotProven
		}
		return domainOwnershipChallengeRecord{}, err
	}
	return domainOwnershipChallengeRecord{
		Hostname: hostname, ProjectID: projectID,
		RecordName:  domainChallengeRecordName(hostname),
		RecordValue: domainChallengeRecordValue(token),
		ExpiresAt:   expiresAt,
	}, nil
}

func (s *Store) deleteDomainOwnershipChallenge(ctx context.Context, projectID, hostname string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM domain_ownership_challenges WHERE hostname = $1 AND project_id = $2`,
		hostname, projectID,
	)
	return err
}

func (s *PlatformService) verifyDomainOwnership(ctx context.Context, subject, projectID, hostname string) error {
	challenge, err := s.store.domainOwnershipChallenge(ctx, subject, projectID, hostname)
	if err != nil {
		return err
	}
	records, err := s.dnsResolver.LookupTXT(ctx, challenge.RecordName)
	if err != nil {
		return fmt.Errorf("%w: lookup %s: %v", errDomainOwnershipNotProven, challenge.RecordName, err)
	}
	for _, record := range records {
		if strings.TrimSpace(record) == challenge.RecordValue {
			return nil
		}
	}
	return fmt.Errorf("%w: expected %s", errDomainOwnershipNotProven, challenge.RecordName)
}
