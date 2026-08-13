package controlplane

import (
	"context"
	"crypto/x509"
	"errors"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type serviceCallerContextKey struct{}
type delegatedUserContextKey struct{}

type serviceCallerClass string

const (
	serviceCallerAgent     serviceCallerClass = "agent"
	serviceCallerBuilder   serviceCallerClass = "builder"
	serviceCallerDashboard serviceCallerClass = "dashboard"
)

const (
	userAssertionHeader    = "x-platform-user-assertion"
	userAssertionIssuer    = "managed-dashboard"
	userAssertionAudience  = "controlplane"
	userAssertionMaxAge    = 30 * time.Second
	userAssertionClockSkew = 5 * time.Second
	maxUserAssertionLength = 4096
)

type ServiceCaller struct {
	Class serviceCallerClass
	ID    string
}

type DelegatedUser struct {
	UserID string
}

type InternalAuth struct {
	dashboardCallerID   string
	userAssertionSecret []byte
	revocations         *CertificateRevocations
	now                 func() time.Time
}

func NewInternalAuth(dashboardCallerID, userAssertionSecret string, revocations ...*CertificateRevocations) *InternalAuth {
	auth := &InternalAuth{
		dashboardCallerID:   strings.TrimSpace(dashboardCallerID),
		userAssertionSecret: []byte(userAssertionSecret),
		now:                 time.Now,
	}
	if len(revocations) > 0 {
		auth.revocations = revocations[0]
	}
	return auth
}

func (a *InternalAuth) UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, err := a.authorize(ctx, info.FullMethod, false)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

func (a *InternalAuth) StreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, err := a.authorize(stream.Context(), info.FullMethod, true)
		if err != nil {
			return err
		}
		return handler(srv, &wrappedServerStream{ServerStream: stream, ctx: ctx})
	}
}

func (a *InternalAuth) authorize(ctx context.Context, fullMethod string, isStream bool) (context.Context, error) {
	if fullMethod == "" || fullMethod[0] != '/' {
		return nil, status.Error(codes.Unimplemented, "malformed method name")
	}
	caller, authenticated, err := authenticatedServiceCallerFromContext(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "peer identity: %v", err)
	}
	if authenticated && a.revocations != nil {
		if err := checkClientCertificateRevocation(a.revocations, verifiedClientCertificateFromContext(ctx)); err != nil {
			return nil, err
		}
	}

	if authenticated {
		ctx = context.WithValue(ctx, serviceCallerContextKey{}, caller)
	}
	if authenticated && caller.Class == serviceCallerDashboard &&
		(a.dashboardCallerID == "" || caller.ID != a.dashboardCallerID) {
		return nil, status.Error(codes.PermissionDenied, "dashboard client certificate common name is not allowed")
	}

	delegatedUser, delegated, err := a.delegatedUserFromMetadata(ctx)
	if err != nil {
		return nil, err
	}
	if delegated {
		if !authenticated || caller.Class != serviceCallerDashboard {
			return nil, status.Error(codes.PermissionDenied, "delegated user metadata requires dashboard caller")
		}
		ctx = context.WithValue(ctx, delegatedUserContextKey{}, delegatedUser)
	}

	switch {
	case strings.HasPrefix(fullMethod, "/platform.v1.PlatformService/"):
		if !authenticated || caller.Class != serviceCallerDashboard {
			return nil, status.Error(codes.PermissionDenied, "dashboard client certificate required")
		}
		if !delegated {
			return nil, status.Error(codes.Unauthenticated, "delegated user metadata is required")
		}
	case strings.HasPrefix(fullMethod, "/platform.v1.OpsService/"):
		if !authenticated || caller.Class != serviceCallerDashboard {
			return nil, status.Error(codes.PermissionDenied, "dashboard client certificate required")
		}
	case strings.HasPrefix(fullMethod, "/platform.v1.BuilderService/"):
		if !authenticated || caller.Class != serviceCallerBuilder {
			return nil, status.Error(codes.PermissionDenied, "builder client certificate required")
		}
	case fullMethod == "/agent.v1.AgentControl/Enroll":
		if authenticated && caller.Class != serviceCallerAgent {
			return nil, status.Error(codes.PermissionDenied, "agent client certificate required")
		}
	case strings.HasPrefix(fullMethod, "/agent.v1.AgentControl/"):
		if !authenticated || caller.Class != serviceCallerAgent {
			return nil, status.Error(codes.PermissionDenied, "agent client certificate required")
		}
	default:
		return nil, status.Error(codes.PermissionDenied, "unsupported method")
	}

	return ctx, nil
}

func ServiceCallerFromContext(ctx context.Context) (ServiceCaller, error) {
	value := ctx.Value(serviceCallerContextKey{})
	caller, ok := value.(ServiceCaller)
	if ok && caller.ID != "" {
		return caller, nil
	}
	caller, authenticated, err := authenticatedServiceCallerFromContext(ctx)
	if err != nil {
		return ServiceCaller{}, status.Errorf(codes.Unauthenticated, "peer identity: %v", err)
	}
	if !authenticated || caller.ID == "" {
		return ServiceCaller{}, status.Error(codes.Unauthenticated, "service caller missing from context")
	}
	return caller, nil
}

func DelegatedUserFromContext(ctx context.Context) (DelegatedUser, error) {
	value := ctx.Value(delegatedUserContextKey{})
	user, ok := value.(DelegatedUser)
	if !ok || user.UserID == "" {
		return DelegatedUser{}, status.Error(codes.Unauthenticated, "delegated user missing from context")
	}
	return user, nil
}

func authenticatedServiceCallerFromContext(ctx context.Context) (ServiceCaller, bool, error) {
	cert := verifiedClientCertificateFromContext(ctx)
	if cert == nil {
		return ServiceCaller{}, false, nil
	}
	return serviceCallerFromCertificate(cert)
}

func verifiedClientCertificateFromContext(ctx context.Context) *x509.Certificate {
	peerInfo, ok := peer.FromContext(ctx)
	if !ok || peerInfo.AuthInfo == nil {
		return nil
	}
	tlsInfo, ok := peerInfo.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return nil
	}
	if len(tlsInfo.State.VerifiedChains) == 0 || len(tlsInfo.State.VerifiedChains[0]) == 0 {
		return nil
	}
	return tlsInfo.State.VerifiedChains[0][0]
}

func checkClientCertificateRevocation(revocations *CertificateRevocations, cert *x509.Certificate) error {
	if revocations == nil || cert == nil {
		return nil
	}
	if err := revocations.Check(cert); err != nil {
		if errors.Is(err, errClientCertificateRevoked) {
			return status.Error(codes.Unauthenticated, "client certificate is revoked")
		}
		return status.Error(codes.Unavailable, "client certificate revocation status is unavailable")
	}
	return nil
}

func serviceCallerFromCertificate(cert *x509.Certificate) (ServiceCaller, bool, error) {
	if cert == nil {
		return ServiceCaller{}, false, nil
	}
	id := strings.TrimSpace(cert.Subject.CommonName)
	if id == "" {
		return ServiceCaller{}, false, errors.New("client certificate common name is required")
	}
	if len(cert.Subject.OrganizationalUnit) == 0 {
		return ServiceCaller{}, false, errors.New("client certificate organizational unit is required")
	}
	class := serviceCallerClass(strings.TrimSpace(cert.Subject.OrganizationalUnit[0]))
	switch class {
	case serviceCallerAgent, serviceCallerBuilder, serviceCallerDashboard:
		return ServiceCaller{Class: class, ID: id}, true, nil
	default:
		return ServiceCaller{}, false, errors.New("unknown client certificate caller class")
	}
}

func (a *InternalAuth) delegatedUserFromMetadata(ctx context.Context) (DelegatedUser, bool, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return DelegatedUser{}, false, nil
	}
	assertions := md.Get(userAssertionHeader)
	if len(assertions) == 0 {
		return DelegatedUser{}, false, nil
	}
	if len(assertions) != 1 {
		return DelegatedUser{}, false, status.Error(codes.Unauthenticated, "exactly one user assertion is required")
	}
	assertion := assertions[0]
	if assertion == "" || len(assertion) > maxUserAssertionLength || len(a.userAssertionSecret) == 0 {
		return DelegatedUser{}, false, status.Error(codes.Unauthenticated, "invalid user assertion")
	}

	claims := jwt.RegisteredClaims{}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(userAssertionIssuer),
		jwt.WithAudience(userAssertionAudience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(userAssertionClockSkew),
		jwt.WithTimeFunc(a.now),
	)
	token, err := parser.ParseWithClaims(assertion, &claims, func(token *jwt.Token) (any, error) {
		return a.userAssertionSecret, nil
	})
	if err != nil || !token.Valid || claims.ExpiresAt == nil || claims.IssuedAt == nil {
		return DelegatedUser{}, false, status.Error(codes.Unauthenticated, "invalid user assertion")
	}
	if claims.ExpiresAt.Time.Before(claims.IssuedAt.Time) || claims.ExpiresAt.Time.Sub(claims.IssuedAt.Time) > userAssertionMaxAge {
		return DelegatedUser{}, false, status.Error(codes.Unauthenticated, "invalid user assertion lifetime")
	}
	userID := strings.TrimSpace(claims.Subject)
	if userID == "" || userID != claims.Subject || len(userID) > 256 || strings.TrimSpace(claims.ID) == "" {
		return DelegatedUser{}, false, status.Error(codes.Unauthenticated, "invalid user assertion claims")
	}
	return DelegatedUser{UserID: userID}, true, nil
}

type wrappedServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedServerStream) Context() context.Context {
	return w.ctx
}
