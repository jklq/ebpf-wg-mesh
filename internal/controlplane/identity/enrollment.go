package identity

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"log/slog"
	"strings"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type EnrollmentStore interface {
	AuthorizeAgentCredential(context.Context, string) error
	ConsumeOrRecoverAgentBootstrapToken(context.Context, string, string, []byte) error
	RecordAgentCertificate(context.Context, string, string) error
}

type Enrollment struct {
	store     EnrollmentStore
	authority *TLSAuthority
}

func NewEnrollment(store EnrollmentStore, authority *TLSAuthority) *Enrollment {
	if store == nil || authority == nil {
		return nil
	}
	return &Enrollment{store: store, authority: authority}
}

func (e *Enrollment) EnrollAgent(ctx context.Context, req *agentv1.EnrollRequest) (*agentv1.EnrollResponse, error) {
	if e == nil || e.store == nil || e.authority == nil {
		return nil, status.Error(codes.FailedPrecondition, "agent enrollment is not configured")
	}
	if err := CheckClientCertificateRevocation(e.authority.Revocations(), VerifiedClientCertificateFromContext(ctx)); err != nil {
		return nil, err
	}
	caller, authenticated, err := AuthenticatedServiceCallerFromContext(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "peer identity: %v", err)
	}
	if authenticated {
		if caller.Class != CallerAgent {
			return nil, status.Error(codes.PermissionDenied, "agent client certificate required")
		}
		if caller.ID != req.GetAgentId() {
			return nil, status.Error(codes.PermissionDenied, "client certificate does not match agent_id")
		}
	}
	if err := e.store.AuthorizeAgentCredential(ctx, req.GetAgentId()); err != nil {
		return nil, status.Error(codes.PermissionDenied, "agent is not enrolled or its credentials are revoked")
	}
	csr, err := parseClientCSR(req.GetCsrPem())
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(csr.Subject.CommonName) != strings.TrimSpace(req.GetAgentId()) {
		return nil, status.Error(codes.InvalidArgument, "csr common name must match agent_id")
	}
	if !authenticated {
		keyHash, err := PublicKeySHA256(csr.PublicKey)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "csr public key: %v", err)
		}
		if err := e.store.ConsumeOrRecoverAgentBootstrapToken(ctx, req.GetAgentId(), req.GetBootstrapToken(), keyHash); err != nil {
			return nil, status.Error(codes.Unauthenticated, "invalid bootstrap token")
		}
	}
	resp, err := e.authority.Enroll(req)
	if err != nil {
		return nil, err
	}
	serial, err := CertificateSerialFromPEM(resp.GetCertPem())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "record agent certificate: %v", err)
	}
	if err := e.store.RecordAgentCertificate(ctx, req.GetAgentId(), serial); err != nil {
		return nil, status.Errorf(codes.Internal, "record agent certificate: %v", err)
	}
	slog.Info("agent certificate issued", "agent_id", req.GetAgentId(), "authenticated_renewal", authenticated)
	return resp, nil
}

func PublicKeySHA256(publicKey any) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(der)
	return sum[:], nil
}
