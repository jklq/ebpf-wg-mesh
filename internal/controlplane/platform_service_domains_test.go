package controlplane

import (
	"context"
	"ebof-wg-mesh/internal/controlplane/authz"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"
	"ebof-wg-mesh/internal/controlplane/routing"
	"strings"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestPlatformServiceUpdateServiceSkipsIngressRequest(t *testing.T) {
	t.Parallel()

	ingress := &countingIngress{}
	service := NewPlatformService(&fakePlatformStore{}, noopNotifier{}, ingress, &fakePlatformDelivery{
		updateServiceFn: func(ctx context.Context, _ authz.User, serviceID, name string, spec *platformv1.ServiceSpec) (deliverycore.ServiceRecord, bool, error) {
			return deliverycore.ServiceRecord{ID: serviceID, EnvironmentID: "environment-1", AllocatedAgentID: "node-1"}, true, nil
		},
	})

	_, err := service.UpdateService(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.UpdateServiceRequest{
		ServiceId: "service-1",
		Service: &platformv1.ServiceUpdate{
			Spec: directImageServiceSpec("nginx:1.27", nil),
		},
	})
	if err != nil {
		t.Fatalf("UpdateService: %v", err)
	}
	if got := ingress.requests.Load(); got != 0 {
		t.Fatalf("expected no ingress requests, got %d", got)
	}
}

func TestPlatformServiceDeleteServiceRequestsIngressWhenServiceHasDomains(t *testing.T) {
	t.Parallel()

	ingress := &countingIngress{}
	service := NewPlatformService(&fakePlatformStore{}, noopNotifier{}, ingress, &fakePlatformDelivery{
		deleteServiceFn: func(ctx context.Context, _ authz.User, serviceID string) error {
			ingress.RequestSync()
			return nil
		},
	})

	_, err := service.DeleteService(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.DeleteServiceRequest{
		ServiceId: "service-1",
	})
	if err != nil {
		t.Fatalf("DeleteService: %v", err)
	}
	if got := ingress.requests.Load(); got != 1 {
		t.Fatalf("expected 1 ingress request, got %d", got)
	}
}

func TestPlatformServiceDeleteServiceSkipsIngressWhenServiceHasNoDomains(t *testing.T) {
	t.Parallel()

	ingress := &countingIngress{}
	service := NewPlatformService(&fakePlatformStore{}, noopNotifier{}, ingress, &fakePlatformDelivery{})

	_, err := service.DeleteService(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.DeleteServiceRequest{
		ServiceId: "service-1",
	})
	if err != nil {
		t.Fatalf("DeleteService: %v", err)
	}
	if got := ingress.requests.Load(); got != 0 {
		t.Fatalf("expected no ingress requests, got %d", got)
	}
}

func TestPlatformServiceUpdateDomainBindingRequestsIngress(t *testing.T) {
	t.Parallel()

	ingress := &countingIngress{}
	service := NewPlatformService(&fakePlatformStore{
		updateDomainBindingFn: func(ctx context.Context, _ authz.User, hostname, serviceID string, targetPort int32) (deliverycore.DomainBindingRecord, bool, error) {
			return deliverycore.DomainBindingRecord{Hostname: hostname, EnvironmentID: "environment-1", ServiceID: serviceID, TargetPort: targetPort}, true, nil
		},
	}, noopNotifier{}, ingress, nil)

	_, err := service.UpdateDomainBinding(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.UpdateDomainBindingRequest{
		Hostname: "web.example.com",
		Binding:  &platformv1.DomainBindingTarget{ServiceId: "service-1", TargetPort: 8080},
	})
	if err != nil {
		t.Fatalf("UpdateDomainBinding: %v", err)
	}
	if got := ingress.requests.Load(); got != 1 {
		t.Fatalf("expected 1 ingress request, got %d", got)
	}
}

func TestPlatformServiceGenerateDomainBindingCreatesStablePlatformHostname(t *testing.T) {
	t.Parallel()

	store := &fakePlatformStore{
		createPlatformDomainBindingFn: func(ctx context.Context, _ authz.User, hostname, serviceID string, targetPort int32) (deliverycore.DomainBindingRecord, bool, error) {
			return deliverycore.DomainBindingRecord{Hostname: hostname, EnvironmentID: "environment-1", ServiceID: serviceID, TargetPort: targetPort, PlatformGenerated: true}, true, nil
		},
	}
	service := NewPlatformService(store, noopNotifier{}, noopIngress{}, nil, WithPlatformDomainSuffix("platform.example"))

	binding, err := service.GenerateDomainBinding(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.GenerateDomainBindingRequest{
		ServiceId:  "service-1",
		TargetPort: 8080,
	})
	if err != nil {
		t.Fatalf("GenerateDomainBinding: %v", err)
	}
	if !strings.HasSuffix(binding.GetHostname(), ".platform.example") {
		t.Fatalf("expected platform hostname, got %q", binding.GetHostname())
	}
	if !binding.GetPlatformGenerated() {
		t.Fatal("expected generated binding marker")
	}
}

func TestPlatformServiceCreateDomainBindingVerifiesCNAMEToPlatformHostname(t *testing.T) {
	t.Parallel()

	service := NewPlatformService(&fakePlatformStore{
		platformDomainBindingForServiceFn: func(ctx context.Context, _ authz.User, serviceID string) (deliverycore.DomainBindingRecord, error) {
			return deliverycore.DomainBindingRecord{Hostname: "violet-7k3.platform.example", EnvironmentID: "environment-1", ServiceID: serviceID, PlatformGenerated: true}, nil
		},
	}, noopNotifier{}, noopIngress{}, nil, WithPlatformDomainSuffix("platform.example"), WithDomainCNAMEResolver(staticCNAMEResolver{
		"web.example.com": "violet-7k3.platform.example.",
	}))
	binding, err := service.CreateDomainBinding(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.CreateDomainBindingRequest{
		Binding: &platformv1.DomainBindingInput{Hostname: "web.example.com", ServiceId: "service-1", TargetPort: 8080},
	})
	if err != nil {
		t.Fatalf("CreateDomainBinding: %v", err)
	}
	if binding.GetHostname() != "web.example.com" {
		t.Fatalf("unexpected hostname %q", binding.GetHostname())
	}
	if binding.GetOwnershipState() != platformv1.DomainOwnershipState_DOMAIN_OWNERSHIP_STATE_VERIFIED {
		t.Fatalf("expected verified ownership, got %s (%s)", binding.GetOwnershipState(), binding.GetOwnershipMessage())
	}
}

func TestPlatformServiceCreateDomainBindingSucceedsWhenCNAMELookupFails(t *testing.T) {
	t.Parallel()

	service := NewPlatformService(&fakePlatformStore{
		platformDomainBindingForServiceFn: func(ctx context.Context, _ authz.User, serviceID string) (deliverycore.DomainBindingRecord, error) {
			return deliverycore.DomainBindingRecord{Hostname: "violet-7k3.platform.example", EnvironmentID: "environment-1", ServiceID: serviceID, PlatformGenerated: true}, nil
		},
	}, noopNotifier{}, noopIngress{}, nil, WithPlatformDomainSuffix("platform.example"), WithDomainCNAMEResolver(staticCNAMEResolver{}))
	binding, err := service.CreateDomainBinding(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.CreateDomainBindingRequest{
		Binding: &platformv1.DomainBindingInput{Hostname: "me.angeltvedt.com", ServiceId: "service-1", TargetPort: 8080},
	})
	if err != nil {
		t.Fatalf("CreateDomainBinding: %v", err)
	}
	if binding.GetOwnershipState() != platformv1.DomainOwnershipState_DOMAIN_OWNERSHIP_STATE_UNVERIFIED {
		t.Fatalf("expected unverified ownership, got %s", binding.GetOwnershipState())
	}
	if !strings.Contains(binding.GetOwnershipMessage(), "lookup me.angeltvedt.com") {
		t.Fatalf("expected lookup failure message, got %q", binding.GetOwnershipMessage())
	}
}

func TestPlatformServiceCreateDomainBindingVerifiesSharedCanonicalName(t *testing.T) {
	t.Parallel()

	service := NewPlatformService(&fakePlatformStore{
		platformDomainBindingForServiceFn: func(ctx context.Context, _ authz.User, serviceID string) (deliverycore.DomainBindingRecord, error) {
			return deliverycore.DomainBindingRecord{Hostname: "violet-7k3.platform.example", EnvironmentID: "environment-1", ServiceID: serviceID, PlatformGenerated: true}, nil
		},
	}, noopNotifier{}, noopIngress{}, nil, WithPlatformDomainSuffix("platform.example"), WithDomainCNAMEResolver(staticDNSResolver{
		cname: map[string]string{
			"web.example.com":             "edge.cfargotunnel.com.",
			"violet-7k3.platform.example": "edge.cfargotunnel.com.",
		},
	}))
	binding, err := service.CreateDomainBinding(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.CreateDomainBindingRequest{
		Binding: &platformv1.DomainBindingInput{Hostname: "web.example.com", ServiceId: "service-1", TargetPort: 8080},
	})
	if err != nil {
		t.Fatalf("CreateDomainBinding: %v", err)
	}
	if binding.GetOwnershipState() != platformv1.DomainOwnershipState_DOMAIN_OWNERSHIP_STATE_VERIFIED {
		t.Fatalf("expected verified ownership, got %s (%s)", binding.GetOwnershipState(), binding.GetOwnershipMessage())
	}
}

func TestPlatformServiceCreateDomainBindingVerifiesMatchingAddresses(t *testing.T) {
	t.Parallel()

	service := NewPlatformService(&fakePlatformStore{
		platformDomainBindingForServiceFn: func(ctx context.Context, _ authz.User, serviceID string) (deliverycore.DomainBindingRecord, error) {
			return deliverycore.DomainBindingRecord{Hostname: "violet-7k3.platform.example", EnvironmentID: "environment-1", ServiceID: serviceID, PlatformGenerated: true}, nil
		},
	}, noopNotifier{}, noopIngress{}, nil, WithPlatformDomainSuffix("platform.example"), WithDomainCNAMEResolver(staticDNSResolver{
		hosts: map[string][]string{
			"web.example.com":             {"104.21.44.122", "2606:4700:3032::6815:2c7a"},
			"violet-7k3.platform.example": {"172.67.199.150", "104.21.44.122"},
		},
	}))
	binding, err := service.CreateDomainBinding(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.CreateDomainBindingRequest{
		Binding: &platformv1.DomainBindingInput{Hostname: "web.example.com", ServiceId: "service-1", TargetPort: 8080},
	})
	if err != nil {
		t.Fatalf("CreateDomainBinding: %v", err)
	}
	if binding.GetOwnershipState() != platformv1.DomainOwnershipState_DOMAIN_OWNERSHIP_STATE_VERIFIED {
		t.Fatalf("expected verified ownership, got %s (%s)", binding.GetOwnershipState(), binding.GetOwnershipMessage())
	}
}

func TestPlatformServiceCreateDomainBindingRecordsUnverifiedCNAME(t *testing.T) {
	t.Parallel()

	service := NewPlatformService(&fakePlatformStore{
		platformDomainBindingForServiceFn: func(ctx context.Context, _ authz.User, serviceID string) (deliverycore.DomainBindingRecord, error) {
			return deliverycore.DomainBindingRecord{Hostname: "violet-7k3.platform.example", EnvironmentID: "environment-1", ServiceID: serviceID, PlatformGenerated: true}, nil
		},
	}, noopNotifier{}, noopIngress{}, nil, WithPlatformDomainSuffix("platform.example"), WithDomainCNAMEResolver(staticCNAMEResolver{
		"web.example.com": "wrong.platform.example.",
	}))
	binding, err := service.CreateDomainBinding(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.CreateDomainBindingRequest{
		Binding: &platformv1.DomainBindingInput{Hostname: "web.example.com", ServiceId: "service-1", TargetPort: 8080},
	})
	if err != nil {
		t.Fatalf("CreateDomainBinding: %v", err)
	}
	if binding.GetOwnershipState() != platformv1.DomainOwnershipState_DOMAIN_OWNERSHIP_STATE_UNVERIFIED {
		t.Fatalf("expected unverified ownership, got %s", binding.GetOwnershipState())
	}
	if !strings.Contains(binding.GetOwnershipMessage(), "expected violet-7k3.platform.example") {
		t.Fatalf("expected ownership message to name the platform hostname, got %q", binding.GetOwnershipMessage())
	}
}

func TestPlatformServiceListDomainBindingsAnnotatesOwnership(t *testing.T) {
	t.Parallel()

	service := NewPlatformService(&fakePlatformStore{
		platformDomainBindingForServiceFn: func(ctx context.Context, _ authz.User, serviceID string) (deliverycore.DomainBindingRecord, error) {
			return deliverycore.DomainBindingRecord{Hostname: "violet-7k3.platform.example", EnvironmentID: "environment-1", ServiceID: serviceID, PlatformGenerated: true}, nil
		},
		listDomainBindingsFn: func(ctx context.Context, _ authz.User, serviceID string) ([]deliverycore.DomainBindingRecord, error) {
			return []deliverycore.DomainBindingRecord{
				{Hostname: "violet-7k3.platform.example", ProjectID: "project-1", ServiceID: serviceID, TargetPort: 8080, PlatformGenerated: true},
				{Hostname: "web.example.com", ProjectID: "project-1", ServiceID: serviceID, TargetPort: 8080},
			}, nil
		},
	}, noopNotifier{}, noopIngress{}, nil, WithPlatformDomainSuffix("platform.example"), WithDomainCNAMEResolver(staticCNAMEResolver{}))

	resp, err := service.ListDomainBindings(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.ListDomainBindingsRequest{
		ServiceId: "service-1",
	})
	if err != nil {
		t.Fatalf("ListDomainBindings: %v", err)
	}
	if len(resp.GetBindings()) != 2 {
		t.Fatalf("expected 2 bindings, got %d", len(resp.GetBindings()))
	}
	if resp.GetBindings()[0].GetOwnershipState() != platformv1.DomainOwnershipState_DOMAIN_OWNERSHIP_STATE_VERIFIED {
		t.Fatalf("expected generated binding to be verified, got %s", resp.GetBindings()[0].GetOwnershipState())
	}
	if resp.GetBindings()[1].GetOwnershipState() != platformv1.DomainOwnershipState_DOMAIN_OWNERSHIP_STATE_UNVERIFIED {
		t.Fatalf("expected custom binding to be unverified, got %s", resp.GetBindings()[1].GetOwnershipState())
	}
	if !strings.Contains(resp.GetBindings()[1].GetOwnershipMessage(), "lookup web.example.com") {
		t.Fatalf("expected lookup failure message, got %q", resp.GetBindings()[1].GetOwnershipMessage())
	}
}

func TestPlatformServiceCreateDomainBindingRequiresPlatformHostname(t *testing.T) {
	t.Parallel()

	service := NewPlatformService(&fakePlatformStore{createDomainBindingFn: func(context.Context, authz.User, string, string, int32) (deliverycore.DomainBindingRecord, bool, error) {
		return deliverycore.DomainBindingRecord{}, false, routing.ErrPlatformDomainNotGenerated
	}}, noopNotifier{}, noopIngress{}, nil, WithPlatformDomainSuffix("platform.example"))
	_, err := service.CreateDomainBinding(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.CreateDomainBindingRequest{
		Binding: &platformv1.DomainBindingInput{Hostname: "web.example.com", ServiceId: "service-1", TargetPort: 8080},
	})
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %s: %v", got, err)
	}
}

func TestPlatformServiceDeleteDomainBindingRequestsIngress(t *testing.T) {
	t.Parallel()

	ingress := &countingIngress{}
	service := NewPlatformService(&fakePlatformStore{
		deleteDomainBindingFn: func(ctx context.Context, _ authz.User, hostname string) (bool, error) {
			return true, nil
		},
	}, noopNotifier{}, ingress, nil)

	_, err := service.DeleteDomainBinding(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.DeleteDomainBindingRequest{
		Hostname: "web.example.com",
	})
	if err != nil {
		t.Fatalf("DeleteDomainBinding: %v", err)
	}
	if got := ingress.requests.Load(); got != 1 {
		t.Fatalf("expected 1 ingress request, got %d", got)
	}
}
