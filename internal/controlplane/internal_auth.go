package controlplane

import (
	"context"
	"crypto/x509"
	"errors"
	"strings"

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
	delegatedUserSubjectHeader = "x-platform-user-subject"
	delegatedUserEmailHeader   = "x-platform-user-email"
)

type ServiceCaller struct {
	Class serviceCallerClass
	ID    string
}

type DelegatedUser struct {
	Subject string
	Email   string
}

type InternalAuth struct{}

func NewInternalAuth() *InternalAuth {
	return &InternalAuth{}
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

	if authenticated {
		ctx = context.WithValue(ctx, serviceCallerContextKey{}, caller)
	}

	delegatedUser, delegated, err := delegatedUserFromMetadata(ctx)
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
		if !strings.HasSuffix(fullMethod, "/EnsurePrincipal") && !delegated {
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
	if !ok || user.Subject == "" {
		return DelegatedUser{}, status.Error(codes.Unauthenticated, "delegated user missing from context")
	}
	return user, nil
}

func authenticatedServiceCallerFromContext(ctx context.Context) (ServiceCaller, bool, error) {
	peerInfo, ok := peer.FromContext(ctx)
	if !ok || peerInfo.AuthInfo == nil {
		return ServiceCaller{}, false, nil
	}
	tlsInfo, ok := peerInfo.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return ServiceCaller{}, false, nil
	}
	if len(tlsInfo.State.VerifiedChains) == 0 || len(tlsInfo.State.VerifiedChains[0]) == 0 {
		return ServiceCaller{}, false, nil
	}
	return serviceCallerFromCertificate(tlsInfo.State.VerifiedChains[0][0])
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

func delegatedUserFromMetadata(ctx context.Context) (DelegatedUser, bool, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return DelegatedUser{}, false, nil
	}
	subjects := md.Get(delegatedUserSubjectHeader)
	emails := md.Get(delegatedUserEmailHeader)
	if len(subjects) == 0 && len(emails) == 0 {
		return DelegatedUser{}, false, nil
	}
	subject := strings.TrimSpace(firstMetadataValue(subjects))
	email := strings.TrimSpace(firstMetadataValue(emails))
	if subject == "" || email == "" {
		return DelegatedUser{}, false, status.Error(codes.Unauthenticated, "delegated user subject and email are required")
	}
	return DelegatedUser{Subject: subject, Email: email}, true, nil
}

func firstMetadataValue(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

type wrappedServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedServerStream) Context() context.Context {
	return w.ctx
}
