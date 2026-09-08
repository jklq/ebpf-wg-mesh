package controlplane

import (
	"context"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"time"

	identitycore "ebof-wg-mesh/internal/controlplane/identity"
)

type serviceCallerClass = identitycore.CallerClass
type ServiceCaller = identitycore.ServiceCaller
type TLSAuthority = identitycore.TLSAuthority
type ClientIdentityMaterial = identitycore.ClientIdentityMaterial

const (
	serviceCallerAgent      = identitycore.CallerAgent
	serviceCallerBuilder    = identitycore.CallerBuilder
	serviceCallerDashboard  = identitycore.CallerDashboard
	testUserAssertionSecret = "test-control-plane-user-assertion-secret"
	userAssertionHeader     = "x-platform-user-assertion"
	userAssertionIssuer     = "managed-dashboard"
	userAssertionAudience   = "controlplane"
	userAssertionMaxAge     = 30 * time.Second
)

var (
	NewTLSAuthority           = identitycore.NewTLSAuthority
	NewCertificateRevocations = identitycore.NewCertificateRevocations
	DelegatedUserFromContext  = identitycore.DelegatedUserFromContext
	ServiceCallerFromContext  = identitycore.ServiceCallerFromContext
)

func contextWithClientIdentity(class serviceCallerClass, id string) context.Context {
	return identitycore.WithVerifiedClientCertificate(context.Background(), &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: id, OrganizationalUnit: []string{string(class)}},
	})
}

func contextWithCertificate(class serviceCallerClass, id string, serial *big.Int) context.Context {
	return identitycore.WithVerifiedClientCertificate(context.Background(), &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: id, OrganizationalUnit: []string{string(class)}},
	})
}
