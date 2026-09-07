package controlplane

import (
	"context"
	"database/sql"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

func TestPlatformServiceListProjectsUsesDelegatedUser(t *testing.T) {
	t.Parallel()

	store := &fakePlatformStore{
		listProjectsFn: func(ctx context.Context, userID string) ([]projectRecord, error) {
			if userID != "user-1" {
				t.Fatalf("unexpected userID %q", userID)
			}
			return []projectRecord{{
				ID:        "project-1",
				Name:      "demo",
				Kind:      projectKindUser,
				CreatedAt: time.Now().UTC(),
			}}, nil
		},
	}
	service := NewPlatformService(store, noopNotifier{}, noopIngress{}, nil)

	resp, err := service.ListProjects(contextWithDelegatedUser("user-1", "user@example.com"), &emptypb.Empty{})
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(resp.GetProjects()) != 1 || resp.GetProjects()[0].GetId() != "project-1" {
		t.Fatalf("unexpected projects %+v", resp.GetProjects())
	}
}

func TestPlatformServiceApplyDeploymentActionRejectsViewerAndStaleTargets(t *testing.T) {
	t.Parallel()

	t.Run("viewer", func(t *testing.T) {
		service := NewPlatformService(&fakePlatformStore{}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{
			applyDeploymentActionFn: func(context.Context, string, string, platformv1.DeploymentAction, string, string) (deploymentActionResult, error) {
				return deploymentActionResult{}, errDeploymentActionDenied
			},
		})
		_, err := service.ApplyDeploymentAction(
			contextWithDelegatedUser("user-1", ""),
			&platformv1.ApplyDeploymentActionRequest{
				ServiceId: "service-1", DeploymentId: "deploy-1",
				Action: platformv1.DeploymentAction_DEPLOYMENT_ACTION_RESTART, IdempotencyKey: "k1",
			},
		)
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("expected PermissionDenied, got %v", err)
		}
	})

	t.Run("stale", func(t *testing.T) {
		service := NewPlatformService(&fakePlatformStore{}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{
			applyDeploymentActionFn: func(context.Context, string, string, platformv1.DeploymentAction, string, string) (deploymentActionResult, error) {
				return deploymentActionResult{}, errDeploymentStale
			},
		})
		_, err := service.ApplyDeploymentAction(
			contextWithDelegatedUser("user-1", ""),
			&platformv1.ApplyDeploymentActionRequest{
				ServiceId: "service-1", DeploymentId: "deploy-old",
				Action: platformv1.DeploymentAction_DEPLOYMENT_ACTION_CANCEL, IdempotencyKey: "k2",
			},
		)
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("expected FailedPrecondition, got %v", err)
		}
	})
}

func TestPlatformServiceRejectsViewerWrites(t *testing.T) {
	t.Parallel()

	service := NewPlatformService(&fakePlatformStore{
		authorizeProjectWriteFn: func(context.Context, string, string) error {
			return sql.ErrNoRows
		},
	}, noopNotifier{}, noopIngress{}, nil)
	_, err := service.DeleteService(
		contextWithDelegatedUser("user-1", ""),
		&platformv1.DeleteServiceRequest{ServiceId: "service-1"},
	)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", err)
	}
}

func TestPlatformServiceCreateServiceMapsPlacementErrors(t *testing.T) {
	t.Parallel()

	delivery := &fakePlatformDelivery{
		createScheduledServiceFn: func(ctx context.Context, projectID, name string, spec *platformv1.ServiceSpec) (serviceRecord, error) {
			return serviceRecord{}, errNoPlacementAvailable
		},
	}
	service := NewPlatformService(&fakePlatformStore{}, noopNotifier{}, noopIngress{}, delivery)

	_, err := service.CreateService(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.CreateServiceRequest{
		EnvironmentId: "project-1",
		Service: &platformv1.ServiceInput{
			Name: "web",
			Spec: directImageServiceSpec("nginx:1.27", nil),
		},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected FailedPrecondition, got %v", err)
	}
}

func TestPlatformServiceRejectsUnsafeHTTPHealthPaths(t *testing.T) {
	t.Parallel()

	service := NewPlatformService(&fakePlatformStore{}, noopNotifier{}, noopIngress{}, nil)
	for _, path := range []string{"healthz", "//redirect.example/healthz", "/healthz\r\nX-Test: injected"} {
		_, err := service.CreateService(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.CreateServiceRequest{
			EnvironmentId: "project-1",
			Service: &platformv1.ServiceInput{Name: "web", Spec: directImageServiceSpec("nginx:1.27", &platformv1.ServiceRuntime{
				Ports:       runtimePortsFromInts([]int32{8080}),
				HealthCheck: &platformv1.HealthCheck{Type: platformv1.HealthCheck_TYPE_HTTP, Path: path},
			})},
		})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("path %q: expected InvalidArgument, got %v", path, err)
		}
	}
}

func TestPlatformServiceRejectsUnknownHealthCheckType(t *testing.T) {
	t.Parallel()

	service := NewPlatformService(&fakePlatformStore{}, noopNotifier{}, noopIngress{}, nil)
	_, err := service.CreateService(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.CreateServiceRequest{
		EnvironmentId: "project-1",
		Service: &platformv1.ServiceInput{Name: "web", Spec: directImageServiceSpec("nginx:1.27", &platformv1.ServiceRuntime{
			Ports: runtimePortsFromInts([]int32{8080}),
			HealthCheck: &platformv1.HealthCheck{
				Type: platformv1.HealthCheck_Type(99),
				Path: "/healthz",
			},
		})},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

func TestPlatformServiceUpdateServiceMapsConcurrentUpdate(t *testing.T) {
	t.Parallel()

	delivery := &fakePlatformDelivery{
		updateServiceFn: func(ctx context.Context, serviceID, name string, spec *platformv1.ServiceSpec) (serviceRecord, bool, error) {
			return serviceRecord{}, false, errConcurrentUpdate
		},
	}
	service := NewPlatformService(&fakePlatformStore{}, noopNotifier{}, noopIngress{}, delivery)

	_, err := service.UpdateService(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.UpdateServiceRequest{
		ServiceId: "service-1",
		Service: &platformv1.ServiceUpdate{
			Spec: directImageServiceSpec("nginx:1.27", nil),
		},
	})
	if status.Code(err) != codes.Aborted {
		t.Fatalf("expected Aborted, got %v", err)
	}
}

func TestPlatformServiceDeleteVolumeReturnsNotFound(t *testing.T) {
	t.Parallel()

	service := NewPlatformService(&fakePlatformStore{
		listVolumesFn: func(ctx context.Context, userID, projectID string) ([]volumeRecord, error) {
			return []volumeRecord{}, nil
		},
	}, noopNotifier{}, noopIngress{}, nil)

	_, err := service.DeleteVolume(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.DeleteVolumeRequest{
		VolumeId: "missing",
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

func TestPlatformServiceGetProjectMapsMissingProject(t *testing.T) {
	t.Parallel()

	service := NewPlatformService(&fakePlatformStore{
		projectByIDFn: func(ctx context.Context, userID, projectID string) (projectRecord, error) {
			return projectRecord{}, sql.ErrNoRows
		},
	}, noopNotifier{}, noopIngress{}, nil)

	_, err := service.GetProject(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.GetProjectRequest{})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}
