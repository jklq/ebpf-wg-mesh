package controlplane

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestPlatformServiceUpdateServiceSkipsIngressRequest(t *testing.T) {
	t.Parallel()

	ingress := &countingIngress{}
	service := NewPlatformService(&fakePlatformStore{
		updateServiceFn: func(ctx context.Context, userID, projectID, serviceID, name string, spec *platformv1.ServiceSpec) (serviceRecord, bool, error) {
			return serviceRecord{ID: serviceID, EnvironmentID: projectID, AllocatedAgentID: "node-1"}, true, nil
		},
	}, noopNotifier{}, ingress)

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
	service := NewPlatformService(&fakePlatformStore{
		listDomainBindingsFn: func(ctx context.Context, userID, projectID, serviceID string) ([]domainBindingRecord, error) {
			return []domainBindingRecord{{Hostname: "web.example.com", EnvironmentID: projectID, ServiceID: serviceID}}, nil
		},
	}, noopNotifier{}, ingress)

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
	service := NewPlatformService(&fakePlatformStore{
		listDomainBindingsFn: func(ctx context.Context, userID, projectID, serviceID string) ([]domainBindingRecord, error) {
			return nil, nil
		},
	}, noopNotifier{}, ingress)

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
		updateDomainBindingFn: func(ctx context.Context, userID, projectID, hostname, serviceID string, targetPort int32) (domainBindingRecord, bool, error) {
			return domainBindingRecord{Hostname: hostname, EnvironmentID: projectID, ServiceID: serviceID, TargetPort: targetPort}, true, nil
		},
	}, noopNotifier{}, ingress)

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
		createPlatformDomainBindingFn: func(ctx context.Context, userID, projectID, hostname, serviceID string, targetPort int32) (domainBindingRecord, bool, error) {
			return domainBindingRecord{Hostname: hostname, EnvironmentID: projectID, ServiceID: serviceID, TargetPort: targetPort, PlatformGenerated: true}, true, nil
		},
	}
	service := NewPlatformService(store, noopNotifier{}, noopIngress{}, WithPlatformDomainSuffix("platform.example"))

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
		platformDomainBindingForServiceFn: func(ctx context.Context, userID, projectID, serviceID string) (domainBindingRecord, error) {
			return domainBindingRecord{Hostname: "violet-7k3.platform.example", EnvironmentID: projectID, ServiceID: serviceID, PlatformGenerated: true}, nil
		},
	}, noopNotifier{}, noopIngress{}, WithPlatformDomainSuffix("platform.example"), WithDomainCNAMEResolver(staticCNAMEResolver{
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
		platformDomainBindingForServiceFn: func(ctx context.Context, userID, projectID, serviceID string) (domainBindingRecord, error) {
			return domainBindingRecord{Hostname: "violet-7k3.platform.example", EnvironmentID: projectID, ServiceID: serviceID, PlatformGenerated: true}, nil
		},
	}, noopNotifier{}, noopIngress{}, WithPlatformDomainSuffix("platform.example"), WithDomainCNAMEResolver(staticCNAMEResolver{}))
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
		platformDomainBindingForServiceFn: func(ctx context.Context, userID, projectID, serviceID string) (domainBindingRecord, error) {
			return domainBindingRecord{Hostname: "violet-7k3.platform.example", EnvironmentID: projectID, ServiceID: serviceID, PlatformGenerated: true}, nil
		},
	}, noopNotifier{}, noopIngress{}, WithPlatformDomainSuffix("platform.example"), WithDomainCNAMEResolver(staticDNSResolver{
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
		platformDomainBindingForServiceFn: func(ctx context.Context, userID, projectID, serviceID string) (domainBindingRecord, error) {
			return domainBindingRecord{Hostname: "violet-7k3.platform.example", EnvironmentID: projectID, ServiceID: serviceID, PlatformGenerated: true}, nil
		},
	}, noopNotifier{}, noopIngress{}, WithPlatformDomainSuffix("platform.example"), WithDomainCNAMEResolver(staticDNSResolver{
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
		platformDomainBindingForServiceFn: func(ctx context.Context, userID, projectID, serviceID string) (domainBindingRecord, error) {
			return domainBindingRecord{Hostname: "violet-7k3.platform.example", EnvironmentID: projectID, ServiceID: serviceID, PlatformGenerated: true}, nil
		},
	}, noopNotifier{}, noopIngress{}, WithPlatformDomainSuffix("platform.example"), WithDomainCNAMEResolver(staticCNAMEResolver{
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
		platformDomainBindingForServiceFn: func(ctx context.Context, userID, projectID, serviceID string) (domainBindingRecord, error) {
			return domainBindingRecord{Hostname: "violet-7k3.platform.example", EnvironmentID: projectID, ServiceID: serviceID, PlatformGenerated: true}, nil
		},
		listDomainBindingsFn: func(ctx context.Context, userID, projectID, serviceID string) ([]domainBindingRecord, error) {
			return []domainBindingRecord{
				{Hostname: "violet-7k3.platform.example", ProjectID: projectID, ServiceID: serviceID, TargetPort: 8080, PlatformGenerated: true},
				{Hostname: "web.example.com", ProjectID: projectID, ServiceID: serviceID, TargetPort: 8080},
			}, nil
		},
	}, noopNotifier{}, noopIngress{}, WithPlatformDomainSuffix("platform.example"), WithDomainCNAMEResolver(staticCNAMEResolver{}))

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

	service := NewPlatformService(&fakePlatformStore{}, noopNotifier{}, noopIngress{}, WithPlatformDomainSuffix("platform.example"))
	_, err := service.CreateDomainBinding(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.CreateDomainBindingRequest{
		Binding: &platformv1.DomainBindingInput{Hostname: "web.example.com", ServiceId: "service-1", TargetPort: 8080},
	})
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %s: %v", got, err)
	}
}

func TestPlatformServiceDeleteDomainBindingRemovesGeneratedWhenLastCustomDeleted(t *testing.T) {
	t.Parallel()

	bindings := []domainBindingRecord{
		{Hostname: "violet-7k3.platform.example", ServiceID: "service-1", PlatformGenerated: true},
		{Hostname: "web.example.com", ServiceID: "service-1"},
	}
	var deleted []string
	service := NewPlatformService(&fakePlatformStore{
		domainBindingByHostFn: func(ctx context.Context, userID, projectID, hostname string) (domainBindingRecord, error) {
			for _, binding := range bindings {
				if binding.Hostname == hostname {
					return binding, nil
				}
			}
			return domainBindingRecord{}, sql.ErrNoRows
		},
		listDomainBindingsFn: func(ctx context.Context, userID, projectID, serviceID string) ([]domainBindingRecord, error) {
			return append([]domainBindingRecord(nil), bindings...), nil
		},
		deleteDomainBindingFn: func(ctx context.Context, userID, projectID, hostname string) (bool, error) {
			next := make([]domainBindingRecord, 0, len(bindings))
			found := false
			for _, binding := range bindings {
				if binding.Hostname == hostname {
					found = true
					continue
				}
				next = append(next, binding)
			}
			if !found {
				return false, sql.ErrNoRows
			}
			bindings = next
			deleted = append(deleted, hostname)
			return true, nil
		},
	}, noopNotifier{}, noopIngress{})

	if _, err := service.DeleteDomainBinding(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.DeleteDomainBindingRequest{
		Hostname: "web.example.com",
	}); err != nil {
		t.Fatalf("DeleteDomainBinding: %v", err)
	}
	if len(deleted) != 2 || deleted[0] != "web.example.com" || deleted[1] != "violet-7k3.platform.example" {
		t.Fatalf("expected custom then generated deletes, got %v", deleted)
	}
}

func TestPlatformServiceDeleteDomainBindingRejectsGeneratedWhileCustomExists(t *testing.T) {
	t.Parallel()

	deleted := 0
	service := NewPlatformService(&fakePlatformStore{
		domainBindingByHostFn: func(ctx context.Context, userID, projectID, hostname string) (domainBindingRecord, error) {
			return domainBindingRecord{Hostname: hostname, ServiceID: "service-1", PlatformGenerated: true}, nil
		},
		listDomainBindingsFn: func(ctx context.Context, userID, projectID, serviceID string) ([]domainBindingRecord, error) {
			return []domainBindingRecord{
				{Hostname: "violet-7k3.platform.example", ServiceID: serviceID, PlatformGenerated: true},
				{Hostname: "web.example.com", ServiceID: serviceID},
			}, nil
		},
		deleteDomainBindingFn: func(ctx context.Context, userID, projectID, hostname string) (bool, error) {
			deleted++
			return true, nil
		},
	}, noopNotifier{}, noopIngress{})

	_, err := service.DeleteDomainBinding(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.DeleteDomainBindingRequest{
		Hostname: "violet-7k3.platform.example",
	})
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %s: %v", got, err)
	}
	if deleted != 0 {
		t.Fatalf("expected generated domain to stay, got %d deletes", deleted)
	}
}

func TestPlatformServiceDeleteDomainBindingRequestsIngress(t *testing.T) {
	t.Parallel()

	ingress := &countingIngress{}
	service := NewPlatformService(&fakePlatformStore{
		deleteDomainBindingFn: func(ctx context.Context, userID, projectID, hostname string) (bool, error) {
			return true, nil
		},
	}, noopNotifier{}, ingress)

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
