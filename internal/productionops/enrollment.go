package productionops

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/pem"
	"fmt"

	"ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/identity"
	"ebof-wg-mesh/internal/controlplane/signkeys"
	"ebof-wg-mesh/internal/deploy"
)

// Tokens are private, agent-bound and generation-bound. Their root already
// belongs to the independently protected encrypted signing-key closure.
func (r *Runner) agentBootstrapTokens(ctx context.Context, signing *signkeys.Service) (map[string]string, error) {
	material, err := signing.Active(ctx, signkeys.ScopeInternalCA)
	if err != nil {
		return nil, err
	}
	defer clear(material.Private)
	tokens := map[string]string{}
	for _, pl := range r.Plan.Placements {
		if pl.Role != deploy.Agent {
			continue
		}
		mac := hmac.New(sha256.New, material.Private)
		mac.Write([]byte("ebpf-wg-mesh/agent-enrollment/v1/" + r.Plan.Installation.ID + "/" + r.Plan.Generation + "/" + pl.Instance))
		tokens[pl.Instance] = hex.EncodeToString(mac.Sum(nil))
	}
	return tokens, nil
}

// Host-admin provisioning has already enrolled this exact key. Persist that
// consumption before exposing the server so its required bootstrap binding
// cannot issue a second identity for a different key. Native enrollment retries
// retain the same-key recovery semantics used by the control plane.
func recordAgentEnrollment(ctx context.Context, db *sql.DB, id, token string, cert []byte, verify bool) error {
	block, _ := pem.Decode(cert)
	if block == nil || token == "" {
		return fmt.Errorf("agent enrollment requires a bound certificate and token")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err
	}
	if leaf.Subject.CommonName != id || len(leaf.Subject.OrganizationalUnit) != 1 || leaf.Subject.OrganizationalUnit[0] != string(identity.CallerAgent) {
		return fmt.Errorf("agent enrollment certificate has the wrong caller identity")
	}
	key := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	hash := delivery.BootstrapTokenHash(token)
	if !verify {
		if _, err := db.ExecContext(ctx, `INSERT INTO agent_bootstrap_tokens(token_hash,agent_id,origin,created_at,consumed_at,csr_public_key_sha256) VALUES($1,$2,'config',statement_timestamp(),statement_timestamp(),$3) ON CONFLICT(token_hash) DO UPDATE SET consumed_at=COALESCE(agent_bootstrap_tokens.consumed_at,excluded.consumed_at),csr_public_key_sha256=excluded.csr_public_key_sha256 WHERE agent_bootstrap_tokens.agent_id=excluded.agent_id`, hash[:], id, key[:]); err != nil {
			return err
		}
	}
	var enrolled int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM agent_bootstrap_tokens WHERE token_hash=$1 AND agent_id=$2 AND consumed_at IS NOT NULL AND csr_public_key_sha256=$3`, hash[:], id, key[:]).Scan(&enrolled); err != nil {
		return err
	}
	if enrolled != 1 {
		return fmt.Errorf("agent bootstrap identity differs from provisioned enrollment")
	}
	return nil
}
