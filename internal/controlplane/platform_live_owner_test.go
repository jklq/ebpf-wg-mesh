package controlplane

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/controlplane/authz"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type staticLiveOwner struct {
	held bool
	addr string
	err  error
}

func (o staticLiveOwner) Lookup(context.Context) (bool, string, error) {
	return o.held, o.addr, o.err
}

func newPlatformServiceWithOwner(owner staticLiveOwner) *PlatformService {
	return NewPlatformService(
		&fakePlatformStore{},
		noopNotifier{},
		noopIngress{},
		&fakePlatformDelivery{},
		WithPlatformLiveOwner(owner),
	)
}

func TestNonOwnerPlatformRPCsRedirectToLiveOwner(t *testing.T) {
	ctx := contextWithDelegatedUser("user-1", "user@example.com")
	service := newPlatformServiceWithOwner(staticLiveOwner{held: false, addr: "owner.example:9443"})
	want := deliverycore.LiveOwnerRedirectMessage("owner.example:9443")

	calls := []struct {
		name string
		call func(context.Context) error
	}{
		{"CreateProject", func(ctx context.Context) error {
			_, err := service.CreateProject(ctx, &platformv1.CreateProjectRequest{Name: "project"})
			return err
		}},
		{"LinkGitHubRepository", func(ctx context.Context) error {
			_, err := service.LinkGitHubRepository(ctx, &platformv1.LinkGitHubRepositoryRequest{ProjectId: "project-1", RepositorySelector: "owner/repo"})
			return err
		}},
		{"GetServiceStatus", func(ctx context.Context) error {
			_, err := service.GetServiceStatus(ctx, &platformv1.GetServiceStatusRequest{ServiceId: "service-1"})
			return err
		}},
		{"GetService", func(ctx context.Context) error {
			_, err := service.GetService(ctx, &platformv1.GetServiceRequest{ServiceId: "service-1"})
			return err
		}},
		{"ListServices", func(ctx context.Context) error {
			_, err := service.ListServices(ctx, &platformv1.ListServicesRequest{EnvironmentId: "environment-1"})
			return err
		}},
		{"ListAgents", func(ctx context.Context) error {
			_, err := service.ListAgents(ctx, &emptypb.Empty{})
			return err
		}},
		{"CreateService", func(ctx context.Context) error {
			_, err := service.CreateService(ctx, &platformv1.CreateServiceRequest{EnvironmentId: "environment-1"})
			return err
		}},
		{"UpdateService", func(ctx context.Context) error {
			_, err := service.UpdateService(ctx, &platformv1.UpdateServiceRequest{ServiceId: "service-1"})
			return err
		}},
		{"ApplyDeploymentAction", func(ctx context.Context) error {
			_, err := service.ApplyDeploymentAction(ctx, &platformv1.ApplyDeploymentActionRequest{
				ServiceId: "service-1", DeploymentId: "deployment-1", IdempotencyKey: "key-1", Action: platformv1.DeploymentAction_DEPLOYMENT_ACTION_RESTART,
			})
			return err
		}},
		{"ScaleService", func(ctx context.Context) error {
			_, err := service.ScaleService(ctx, &platformv1.ScaleServiceRequest{ServiceId: "service-1", DesiredReplicaCount: 2})
			return err
		}},
		{"DiscardServiceChanges", func(ctx context.Context) error {
			_, err := service.DiscardServiceChanges(ctx, &platformv1.DiscardServiceChangesRequest{ServiceId: "service-1", DiscardAll: true})
			return err
		}},
		{"DeleteService", func(ctx context.Context) error {
			_, err := service.DeleteService(ctx, &platformv1.DeleteServiceRequest{ServiceId: "service-1"})
			return err
		}},
		{"ReleaseEnvironment", func(ctx context.Context) error {
			_, err := service.ReleaseEnvironment(ctx, &platformv1.ReleaseEnvironmentRequest{EnvironmentId: "environment-1"})
			return err
		}},
		{"CreateVolume", func(ctx context.Context) error {
			_, err := service.CreateVolume(ctx, &platformv1.CreateVolumeRequest{EnvironmentId: "environment-1", Name: "data", SizeBytes: 1})
			return err
		}},
		{"DeleteVolume", func(ctx context.Context) error {
			_, err := service.DeleteVolume(ctx, &platformv1.DeleteVolumeRequest{VolumeId: "volume-1"})
			return err
		}},
		{"CreateDomainBinding", func(ctx context.Context) error {
			_, err := service.CreateDomainBinding(ctx, &platformv1.CreateDomainBindingRequest{})
			return err
		}},
		{"GenerateDomainBinding", func(ctx context.Context) error {
			_, err := service.GenerateDomainBinding(ctx, &platformv1.GenerateDomainBindingRequest{ServiceId: "service-1"})
			return err
		}},
		{"UpdateDomainBinding", func(ctx context.Context) error {
			_, err := service.UpdateDomainBinding(ctx, &platformv1.UpdateDomainBindingRequest{Hostname: "web.example.com"})
			return err
		}},
		{"DeleteDomainBinding", func(ctx context.Context) error {
			_, err := service.DeleteDomainBinding(ctx, &platformv1.DeleteDomainBindingRequest{Hostname: "web.example.com"})
			return err
		}},
	}

	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(ctx)
			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("non-owner %s error = %v, want FailedPrecondition redirect", tc.name, err)
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("non-owner %s error = %v, want redirect %q", tc.name, err, want)
			}
		})
	}
}

func TestListAgentsMapsErrNotLiveOwner(t *testing.T) {
	ctx := contextWithDelegatedUser("user-1", "user@example.com")
	store := &fakePlatformStore{listAgentsFn: func(context.Context, authz.User) ([]deliverycore.AgentRecord, error) {
		return nil, deliverycore.ErrNotLiveOwner
	}}
	service := NewPlatformService(store, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{}, WithPlatformLiveOwner(staticLiveOwner{held: true}))
	_, err := service.ListAgents(ctx, &emptypb.Empty{})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("ListAgents = %v, want Unavailable while the lease holder is not serving", err)
	}
}

func TestServiceReadsMapErrNotLiveOwnerAfterInitialOwnerCheck(t *testing.T) {
	ctx := contextWithDelegatedUser("user-1", "user@example.com")

	t.Run("create reload", func(t *testing.T) {
		store := &fakePlatformStore{serviceByIDFn: func(context.Context, authz.User, string) (deliverycore.ServiceRecord, error) {
			return deliverycore.ServiceRecord{}, deliverycore.ErrNotLiveOwner
		}}
		service := NewPlatformService(store, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{}, WithPlatformLiveOwner(staticLiveOwner{held: true}))
		_, err := service.CreateService(ctx, &platformv1.CreateServiceRequest{
			EnvironmentId: "environment-1",
			Service: &platformv1.ServiceInput{
				Name: "web",
				Spec: directImageServiceSpec("busybox:1.36", nil),
			},
		})
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("CreateService = %v, want Unavailable", err)
		}
	})

	t.Run("delete delivery", func(t *testing.T) {
		delivery := &fakePlatformDelivery{deleteServiceFn: func(context.Context, authz.User, string) error {
			return deliverycore.ErrNotLiveOwner
		}}
		service := NewPlatformService(&fakePlatformStore{}, noopNotifier{}, noopIngress{}, delivery, WithPlatformLiveOwner(staticLiveOwner{held: true}))
		_, err := service.DeleteService(ctx, &platformv1.DeleteServiceRequest{ServiceId: "service-1"})
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("DeleteService = %v, want Unavailable", err)
		}
	})
}

func TestCreateVolumeMapsValidationErrors(t *testing.T) {
	ctx := contextWithDelegatedUser("user-1", "user@example.com")
	store := &fakePlatformStore{}
	service := NewPlatformService(store, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{}, WithPlatformLiveOwner(staticLiveOwner{held: true}))

	if _, err := service.CreateVolume(ctx, &platformv1.CreateVolumeRequest{Name: "data", SizeBytes: 1}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty environment: %v", err)
	}
	store.createScheduledVolumeFn = func(context.Context, authz.User, string, string, int64) (deliverycore.VolumeRecord, error) {
		return deliverycore.VolumeRecord{}, sql.ErrNoRows
	}
	if _, err := service.CreateVolume(ctx, &platformv1.CreateVolumeRequest{EnvironmentId: "missing", Name: "data", SizeBytes: 1}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown environment: %v", err)
	}
	store.createScheduledVolumeFn = func(context.Context, authz.User, string, string, int64) (deliverycore.VolumeRecord, error) {
		return deliverycore.VolumeRecord{}, deliverycore.ErrVolumeAlreadyExists
	}
	if _, err := service.CreateVolume(ctx, &platformv1.CreateVolumeRequest{EnvironmentId: "environment-1", Name: "data", SizeBytes: 1}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("duplicate name: %v", err)
	}
	store.createScheduledVolumeFn = func(context.Context, authz.User, string, string, int64) (deliverycore.VolumeRecord, error) {
		return deliverycore.VolumeRecord{}, deliverycore.ErrInvalidVolume
	}
	if _, err := service.CreateVolume(ctx, &platformv1.CreateVolumeRequest{EnvironmentId: "environment-1", Name: "data", SizeBytes: 1}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("store validation: %v", err)
	}
}

func TestLiveOwnerPlatformRPCsAreNotRedirected(t *testing.T) {
	ctx := contextWithDelegatedUser("user-1", "user@example.com")
	service := newPlatformServiceWithOwner(staticLiveOwner{held: true, addr: "owner.example:9443"})
	if _, err := service.ListAgents(ctx, &emptypb.Empty{}); err != nil {
		t.Fatalf("owner ListAgents error = %v, want nil", err)
	}
}
