package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"ebof-wg-mesh/internal/controlplane/authz"
	deliverycore "ebof-wg-mesh/internal/controlplane/delivery"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestPlatformServiceListProjectsUsesDelegatedUser(t *testing.T) {
	t.Parallel()

	store := &fakePlatformStore{
		listProjectsFn: func(ctx context.Context, user authz.User, includeDeleted bool) ([]deliverycore.ProjectRecord, error) {
			if user.ID() != "user-1" {
				t.Fatalf("unexpected user %q", user.ID())
			}
			return []deliverycore.ProjectRecord{{
				ID:        "project-1",
				Name:      "demo",
				Kind:      deliverycore.ProjectKindUser,
				CreatedAt: time.Now().UTC(),
			}}, nil
		},
	}
	service := NewPlatformService(store, noopNotifier{}, noopIngress{}, nil)

	resp, err := service.ListProjects(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.ListProjectsRequest{})
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
			applyDeploymentActionFn: func(context.Context, authz.User, string, string, platformv1.DeploymentAction, string, string) (deliverycore.DeploymentActionResult, error) {
				return deliverycore.DeploymentActionResult{}, authz.ErrDenied
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
			applyDeploymentActionFn: func(context.Context, authz.User, string, string, platformv1.DeploymentAction, string, string) (deliverycore.DeploymentActionResult, error) {
				return deliverycore.DeploymentActionResult{}, deliverycore.ErrDeploymentStale
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

	service := NewPlatformService(&fakePlatformStore{}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{
		deleteServiceFn: func(context.Context, authz.User, string) error {
			return authz.ErrDenied
		},
	})
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
		createScheduledServiceFn: func(ctx context.Context, _ authz.User, environmentID, name string, spec *platformv1.ServiceSpec) (deliverycore.ServiceRecord, error) {
			return deliverycore.ServiceRecord{}, deliverycore.ErrNoPlacementAvailable
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
		updateServiceFn: func(ctx context.Context, _ authz.User, serviceID, name string, spec *platformv1.ServiceSpec) (deliverycore.ServiceRecord, bool, error) {
			return deliverycore.ServiceRecord{}, false, deliverycore.ErrConcurrentUpdate
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
		listVolumesFn: func(ctx context.Context, _ authz.User, _ string, _ bool) ([]deliverycore.VolumeRecord, error) {
			return []deliverycore.VolumeRecord{}, nil
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
		projectByIDFn: func(ctx context.Context, _ authz.User, _ string) (deliverycore.ProjectRecord, error) {
			return deliverycore.ProjectRecord{}, sql.ErrNoRows
		},
	}, noopNotifier{}, noopIngress{}, nil)

	_, err := service.GetProject(contextWithDelegatedUser("user-1", "user@example.com"), &platformv1.GetProjectRequest{})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("expected NotFound, got %v", err)
	}
}

// TestPlatformServiceAccessErrorCodes pins the per-RPC-class denial policy:
// writes and lists map ErrDenied to PermissionDenied, single reads map it to
// NotFound, plain missing rows are NotFound, and anything else is Internal.
func TestPlatformServiceAccessErrorCodes(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")
	denied := fmt.Errorf("authz: access denied: %w: %w", authz.ErrDenied, sql.ErrNoRows)
	ctx := contextWithDelegatedUser("user-1", "user@example.com")
	spec := func() *platformv1.ServiceSpec { return directImageServiceSpec("nginx:1.27", nil) }

	cases := []struct {
		name    string
		fail    error
		want    codes.Code
		service func(fail error) *PlatformService
		call    func(*PlatformService) error
	}{
		{
			name: "create environment denied", fail: denied, want: codes.PermissionDenied,
			service: func(fail error) *PlatformService {
				return NewPlatformService(&fakePlatformStore{
					createEnvironmentFn: func(context.Context, authz.User, string, string) (deliverycore.EnvironmentRecord, error) {
						return deliverycore.EnvironmentRecord{}, fail
					},
				}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{})
			},
			call: func(s *PlatformService) error {
				_, err := s.CreateEnvironment(ctx, &platformv1.CreateEnvironmentRequest{ProjectId: "project-1", Name: "preview"})
				return err
			},
		},
		{
			name: "create environment missing", fail: sql.ErrNoRows, want: codes.NotFound,
			service: func(fail error) *PlatformService {
				return NewPlatformService(&fakePlatformStore{
					createEnvironmentFn: func(context.Context, authz.User, string, string) (deliverycore.EnvironmentRecord, error) {
						return deliverycore.EnvironmentRecord{}, fail
					},
				}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{})
			},
			call: func(s *PlatformService) error {
				_, err := s.CreateEnvironment(ctx, &platformv1.CreateEnvironmentRequest{ProjectId: "project-1", Name: "preview"})
				return err
			},
		},
		{
			name: "create environment fault", fail: boom, want: codes.Internal,
			service: func(fail error) *PlatformService {
				return NewPlatformService(&fakePlatformStore{
					createEnvironmentFn: func(context.Context, authz.User, string, string) (deliverycore.EnvironmentRecord, error) {
						return deliverycore.EnvironmentRecord{}, fail
					},
				}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{})
			},
			call: func(s *PlatformService) error {
				_, err := s.CreateEnvironment(ctx, &platformv1.CreateEnvironmentRequest{ProjectId: "project-1", Name: "preview"})
				return err
			},
		},
		{
			name: "duplicate environment denied", fail: denied, want: codes.PermissionDenied,
			service: func(fail error) *PlatformService {
				return NewPlatformService(&fakePlatformStore{}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{
					duplicateEnvironmentFn: func(context.Context, authz.User, string, string, bool) (deliverycore.EnvironmentRecord, error) {
						return deliverycore.EnvironmentRecord{}, fail
					},
				})
			},
			call: func(s *PlatformService) error {
				_, err := s.DuplicateEnvironment(ctx, &platformv1.DuplicateEnvironmentRequest{SourceEnvironmentId: "env-1", Name: "copy"})
				return err
			},
		},
		{
			name: "rename environment denied", fail: denied, want: codes.PermissionDenied,
			service: func(fail error) *PlatformService {
				return NewPlatformService(&fakePlatformStore{
					renameEnvironmentFn: func(context.Context, authz.User, string, string) (deliverycore.EnvironmentRecord, error) {
						return deliverycore.EnvironmentRecord{}, fail
					},
				}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{})
			},
			call: func(s *PlatformService) error {
				_, err := s.RenameEnvironment(ctx, &platformv1.RenameEnvironmentRequest{EnvironmentId: "env-1", Name: "next"})
				return err
			},
		},
		{
			name: "update environment auto-deploy denied", fail: denied, want: codes.PermissionDenied,
			service: func(fail error) *PlatformService {
				return NewPlatformService(&fakePlatformStore{
					updateEnvironmentAutoDeployFn: func(context.Context, authz.User, string, bool) (deliverycore.EnvironmentRecord, error) {
						return deliverycore.EnvironmentRecord{}, fail
					},
				}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{})
			},
			call: func(s *PlatformService) error {
				_, err := s.UpdateEnvironmentAutoDeploy(ctx, &platformv1.UpdateEnvironmentAutoDeployRequest{EnvironmentId: "env-1", AutoDeploy: true})
				return err
			},
		},
		{
			name: "delete environment denied", fail: denied, want: codes.PermissionDenied,
			service: func(fail error) *PlatformService {
				return NewPlatformService(&fakePlatformStore{
					deleteEnvironmentFn: func(context.Context, authz.User, string, string) ([]string, error) {
						return nil, fail
					},
				}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{})
			},
			call: func(s *PlatformService) error {
				_, err := s.DeleteEnvironment(ctx, &platformv1.DeleteEnvironmentRequest{EnvironmentId: "env-1"})
				return err
			},
		},
		{
			name: "delete environment missing", fail: sql.ErrNoRows, want: codes.NotFound,
			service: func(fail error) *PlatformService {
				return NewPlatformService(&fakePlatformStore{
					deleteEnvironmentFn: func(context.Context, authz.User, string, string) ([]string, error) {
						return nil, fail
					},
				}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{})
			},
			call: func(s *PlatformService) error {
				_, err := s.DeleteEnvironment(ctx, &platformv1.DeleteEnvironmentRequest{EnvironmentId: "env-1"})
				return err
			},
		},
		{
			name: "release environment denied", fail: denied, want: codes.PermissionDenied,
			service: func(fail error) *PlatformService {
				return NewPlatformService(&fakePlatformStore{}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{
					releaseEnvironmentFn: func(context.Context, authz.User, string) ([]deliverycore.ReleasedService, error) {
						return nil, fail
					},
				})
			},
			call: func(s *PlatformService) error {
				_, err := s.ReleaseEnvironment(ctx, &platformv1.ReleaseEnvironmentRequest{EnvironmentId: "env-1"})
				return err
			},
		},
		{
			name: "release environment missing", fail: sql.ErrNoRows, want: codes.NotFound,
			service: func(fail error) *PlatformService {
				return NewPlatformService(&fakePlatformStore{}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{
					releaseEnvironmentFn: func(context.Context, authz.User, string) ([]deliverycore.ReleasedService, error) {
						return nil, fail
					},
				})
			},
			call: func(s *PlatformService) error {
				_, err := s.ReleaseEnvironment(ctx, &platformv1.ReleaseEnvironmentRequest{EnvironmentId: "env-1"})
				return err
			},
		},
		{
			name: "create service write denied", fail: denied, want: codes.PermissionDenied,
			service: func(fail error) *PlatformService {
				return NewPlatformService(&fakePlatformStore{}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{
					createScheduledServiceFn: func(context.Context, authz.User, string, string, *platformv1.ServiceSpec) (deliverycore.ServiceRecord, error) {
						return deliverycore.ServiceRecord{}, fail
					},
				})
			},
			call: func(s *PlatformService) error {
				_, err := s.CreateService(ctx, &platformv1.CreateServiceRequest{
					EnvironmentId: "env-1",
					Service:       &platformv1.ServiceInput{Name: "web", Spec: spec()},
				})
				return err
			},
		},
		{
			name: "update service write denied", fail: denied, want: codes.PermissionDenied,
			service: func(fail error) *PlatformService {
				return NewPlatformService(&fakePlatformStore{}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{
					updateServiceFn: func(context.Context, authz.User, string, string, *platformv1.ServiceSpec) (deliverycore.ServiceRecord, bool, error) {
						return deliverycore.ServiceRecord{}, false, fail
					},
				})
			},
			call: func(s *PlatformService) error {
				_, err := s.UpdateService(ctx, &platformv1.UpdateServiceRequest{
					ServiceId: "service-1",
					Service:   &platformv1.ServiceUpdate{Spec: spec()},
				})
				return err
			},
		},
		{
			name: "scale service write denied", fail: denied, want: codes.PermissionDenied,
			service: func(fail error) *PlatformService {
				return NewPlatformService(&fakePlatformStore{}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{
					scaleServiceFn: func(context.Context, authz.User, string, int32) (deliverycore.ServiceRecord, []deliverycore.AllocationRecord, int64, error) {
						return deliverycore.ServiceRecord{}, nil, 0, fail
					},
				})
			},
			call: func(s *PlatformService) error {
				_, err := s.ScaleService(ctx, &platformv1.ScaleServiceRequest{ServiceId: "service-1", DesiredReplicaCount: 2})
				return err
			},
		},
		{
			name: "discard service changes denied", fail: denied, want: codes.PermissionDenied,
			service: func(fail error) *PlatformService {
				return NewPlatformService(&fakePlatformStore{}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{
					discardServiceChangesFn: func(context.Context, authz.User, string, []string, bool) (deliverycore.ServiceRecord, error) {
						return deliverycore.ServiceRecord{}, fail
					},
				})
			},
			call: func(s *PlatformService) error {
				_, err := s.DiscardServiceChanges(ctx, &platformv1.DiscardServiceChangesRequest{ServiceId: "service-1", DiscardAll: true})
				return err
			},
		},
		{
			name: "delete service denied", fail: denied, want: codes.PermissionDenied,
			service: func(fail error) *PlatformService {
				return NewPlatformService(&fakePlatformStore{}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{
					deleteServiceFn: func(context.Context, authz.User, string) error { return fail },
				})
			},
			call: func(s *PlatformService) error {
				_, err := s.DeleteService(ctx, &platformv1.DeleteServiceRequest{ServiceId: "service-1"})
				return err
			},
		},
		{
			name: "list volumes denied", fail: denied, want: codes.PermissionDenied,
			service: func(fail error) *PlatformService {
				return NewPlatformService(&fakePlatformStore{
					listVolumesFn: func(context.Context, authz.User, string, bool) ([]deliverycore.VolumeRecord, error) {
						return nil, fail
					},
				}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{})
			},
			call: func(s *PlatformService) error {
				_, err := s.ListVolumes(ctx, &platformv1.ListVolumesRequest{EnvironmentId: "env-1"})
				return err
			},
		},
		{
			name: "list domain bindings denied", fail: denied, want: codes.PermissionDenied,
			service: func(fail error) *PlatformService {
				return NewPlatformService(&fakePlatformStore{
					listDomainBindingsFn: func(context.Context, authz.User, string, bool) ([]deliverycore.DomainBindingRecord, error) {
						return nil, fail
					},
				}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{})
			},
			call: func(s *PlatformService) error {
				_, err := s.ListDomainBindings(ctx, &platformv1.ListDomainBindingsRequest{ServiceId: "service-1"})
				return err
			},
		},
		{
			name: "list environments denied", fail: denied, want: codes.PermissionDenied,
			service: func(fail error) *PlatformService {
				return NewPlatformService(&fakePlatformStore{
					listEnvironmentsFn: func(context.Context, authz.User, string, bool) ([]deliverycore.EnvironmentRecord, error) {
						return nil, fail
					},
				}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{})
			},
			call: func(s *PlatformService) error {
				_, err := s.ListEnvironments(ctx, &platformv1.ListEnvironmentsRequest{ProjectId: "project-1"})
				return err
			},
		},
		{
			name: "list services pre-wait denied", fail: denied, want: codes.PermissionDenied,
			service: func(fail error) *PlatformService {
				return NewPlatformService(&fakePlatformStore{
					environmentByIDFn: func(context.Context, authz.User, string) (deliverycore.EnvironmentRecord, error) {
						return deliverycore.EnvironmentRecord{}, fail
					},
				}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{})
			},
			call: func(s *PlatformService) error {
				_, err := s.ListServices(ctx, &platformv1.ListServicesRequest{EnvironmentId: "env-1"})
				return err
			},
		},
		{
			name: "get service denied reads as not found", fail: denied, want: codes.NotFound,
			service: func(fail error) *PlatformService {
				return NewPlatformService(&fakePlatformStore{
					serviceByIDFn: func(context.Context, authz.User, string) (deliverycore.ServiceRecord, error) {
						return deliverycore.ServiceRecord{}, fail
					},
				}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{})
			},
			call: func(s *PlatformService) error {
				_, err := s.GetService(ctx, &platformv1.GetServiceRequest{ServiceId: "service-1"})
				return err
			},
		},
		{
			name: "get service fault", fail: boom, want: codes.Internal,
			service: func(fail error) *PlatformService {
				return NewPlatformService(&fakePlatformStore{
					serviceByIDFn: func(context.Context, authz.User, string) (deliverycore.ServiceRecord, error) {
						return deliverycore.ServiceRecord{}, fail
					},
				}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{})
			},
			call: func(s *PlatformService) error {
				_, err := s.GetService(ctx, &platformv1.GetServiceRequest{ServiceId: "service-1"})
				return err
			},
		},
		{
			name: "get environment denied reads as not found", fail: denied, want: codes.NotFound,
			service: func(fail error) *PlatformService {
				return NewPlatformService(&fakePlatformStore{
					environmentByIDFn: func(context.Context, authz.User, string) (deliverycore.EnvironmentRecord, error) {
						return deliverycore.EnvironmentRecord{}, fail
					},
				}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{})
			},
			call: func(s *PlatformService) error {
				_, err := s.GetEnvironment(ctx, &platformv1.GetEnvironmentRequest{EnvironmentId: "env-1"})
				return err
			},
		},
		{
			name: "get project denied reads as not found", fail: denied, want: codes.NotFound,
			service: func(fail error) *PlatformService {
				return NewPlatformService(&fakePlatformStore{
					projectByIDFn: func(context.Context, authz.User, string) (deliverycore.ProjectRecord, error) {
						return deliverycore.ProjectRecord{}, fail
					},
				}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{})
			},
			call: func(s *PlatformService) error {
				_, err := s.GetProject(ctx, &platformv1.GetProjectRequest{ProjectId: "project-1"})
				return err
			},
		},
		{
			name: "delete volume denied", fail: denied, want: codes.PermissionDenied,
			service: func(fail error) *PlatformService {
				return NewPlatformService(&fakePlatformStore{
					deleteVolumeFn: func(context.Context, authz.User, string, string) error { return fail },
				}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{})
			},
			call: func(s *PlatformService) error {
				_, err := s.DeleteVolume(ctx, &platformv1.DeleteVolumeRequest{VolumeId: "volume-1"})
				return err
			},
		},
		{
			name: "create volume denied", fail: denied, want: codes.PermissionDenied,
			service: func(fail error) *PlatformService {
				return NewPlatformService(&fakePlatformStore{
					createScheduledVolumeFn: func(context.Context, authz.User, string, string, int64) (deliverycore.VolumeRecord, error) {
						return deliverycore.VolumeRecord{}, fail
					},
				}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{})
			},
			call: func(s *PlatformService) error {
				_, err := s.CreateVolume(ctx, &platformv1.CreateVolumeRequest{EnvironmentId: "env-1", Name: "data", SizeBytes: 64 << 20})
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := tc.call(tc.service(tc.fail)); status.Code(err) != tc.want {
				t.Fatalf("code = %v, want %v (err: %v)", status.Code(err), tc.want, err)
			}
		})
	}
}

func TestPlatformServiceRejectsBlankDelegatedUser(t *testing.T) {
	t.Parallel()

	service := NewPlatformService(&fakePlatformStore{}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{})
	for _, id := range []string{"", "   "} {
		_, err := service.ListProjects(contextWithDelegatedUser(id, ""), &platformv1.ListProjectsRequest{})
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("id %q: code = %v, want Unauthenticated (err: %v)", id, status.Code(err), err)
		}
	}
}

func TestServiceStatusErrorCodes(t *testing.T) {
	t.Parallel()

	service := NewPlatformService(&fakePlatformStore{}, noopNotifier{}, noopIngress{}, &fakePlatformDelivery{})
	ctx := context.Background()
	denied := fmt.Errorf("%w: %w", authz.ErrDenied, sql.ErrNoRows)
	for _, tc := range []struct {
		name string
		fail error
		want codes.Code
	}{
		{"denied", denied, codes.NotFound},
		{"missing", sql.ErrNoRows, codes.NotFound},
		{"fault", errors.New("boom"), codes.Internal},
	} {
		if err := service.serviceStatusError(ctx, tc.fail); status.Code(err) != tc.want {
			t.Fatalf("%s: code = %v, want %v", tc.name, status.Code(err), tc.want)
		}
	}
}

func TestFleetStatusErrorCodes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		fail error
		want codes.Code
	}{
		{"denied", fmt.Errorf("%w: %w", authz.ErrDenied, sql.ErrNoRows), codes.PermissionDenied},
		{"missing", sql.ErrNoRows, codes.PermissionDenied},
		{"invalid input", fmt.Errorf("create agent: %w", deliverycore.ErrInvalidFleetAgentInput), codes.InvalidArgument},
		{"bad transition", deliverycore.ErrInvalidAgentTransition, codes.FailedPrecondition},
		{"has allocations", deliverycore.ErrAgentHasAllocations, codes.FailedPrecondition},
		{"fault", errors.New("boom"), codes.Internal},
	} {
		if err := fleetStatusError("test", tc.fail); status.Code(err) != tc.want {
			t.Fatalf("%s: code = %v, want %v", tc.name, status.Code(err), tc.want)
		}
	}
}
