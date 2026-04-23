//go:build integration

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"
	"ebof-wg-mesh/internal/config"
)

func TestDesiredStateForAgentIncludesVolumeBoundService(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)

	ctx := context.Background()
	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{Subject: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := store.upsertAgent(ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}

	volume, err := store.createVolume(ctx, "user-1", projects[0].ID, "data", 64<<20, "node-1")
	if err != nil {
		t.Fatalf("createVolume: %v", err)
	}
	_, err = store.createService(ctx, "user-1", projects[0].ID, "web", directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		CpuMillis:       100,
		MemoryMebibytes: 64,
		Ports:           runtimePortsFromInts([]int32{8080}),
		VolumeName:      "data",
	}), "node-1")
	if err != nil {
		t.Fatalf("createService: %v", err)
	}

	stateCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	state, err := store.desiredStateForAgent(stateCtx, "node-1")
	if err != nil {
		t.Fatalf("desiredStateForAgent: %v", err)
	}
	if len(state.GetServices()) != 1 {
		t.Fatalf("expected 1 desired service, got %d", len(state.GetServices()))
	}
	if got := state.GetNodeConfig().GetWorkloadIpv6Subnet(); got != "fd00:200:0:1::/64" {
		t.Fatalf("expected assigned workload subnet fd00:200:0:1::/64, got %q", got)
	}
	if got := state.GetServices()[0].GetVolumeId(); got != volume.ID {
		t.Fatalf("expected volume %q, got %q", volume.ID, got)
	}
}

func TestDeleteVolumeRejectsReferencedService(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)

	ctx := context.Background()
	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{Subject: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := store.upsertAgent(ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}

	volume, err := store.createVolume(ctx, "user-1", projects[0].ID, "data", 64<<20, "node-1")
	if err != nil {
		t.Fatalf("createVolume: %v", err)
	}
	if _, err := store.createService(ctx, "user-1", projects[0].ID, "web", directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
		VolumeName: "data",
	}), "node-1"); err != nil {
		t.Fatalf("createService: %v", err)
	}

	err = store.deleteVolume(ctx, "user-1", projects[0].ID, volume.ID)
	if !errors.Is(err, errVolumeInUse) {
		t.Fatalf("expected errVolumeInUse, got %v", err)
	}
}

func TestConcurrentDeleteVolumeAndCreateServiceStayConsistent(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)

	ctx := context.Background()
	if err := store.EnsureBootstrap(ctx, config.BootstrapConfig{
		Users: []config.BootstrapUser{{Subject: "user-1", Email: "user@example.com", Projects: []string{"demo"}}},
	}); err != nil {
		t.Fatal(err)
	}
	projects, err := store.listProjects(ctx, "user-1")
	if err != nil || len(projects) != 1 {
		t.Fatalf("listProjects: %v", err)
	}
	if _, err := store.upsertAgent(ctx, agentHello("node-1")); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 25; i++ {
		volumeName := fmt.Sprintf("data-%d", i)
		serviceName := fmt.Sprintf("svc-%d", i)
		volume, err := store.createVolume(ctx, "user-1", projects[0].ID, volumeName, 64<<20, "node-1")
		if err != nil {
			t.Fatalf("createVolume(%d): %v", i, err)
		}

		start := make(chan struct{})
		createErrCh := make(chan error, 1)
		deleteErrCh := make(chan error, 1)

		go func() {
			<-start
			_, err := store.createScheduledService(ctx, "user-1", projects[0].ID, serviceName, directImageServiceSpec("busybox:1.36", &platformv1.ServiceRuntime{
				CpuMillis:       100,
				MemoryMebibytes: 64,
				Ports:           runtimePortsFromInts([]int32{8080}),
				VolumeName:      volumeName,
			}))
			createErrCh <- err
		}()
		go func() {
			<-start
			deleteErrCh <- store.deleteVolume(ctx, "user-1", projects[0].ID, volume.ID)
		}()

		close(start)
		createErr := <-createErrCh
		deleteErr := <-deleteErrCh
		if createErr == nil && deleteErr == nil {
			t.Fatalf("iteration %d: create and delete both succeeded for volume %q", i, volumeName)
		}

		services, err := store.listServices(ctx, "user-1", projects[0].ID)
		if err != nil {
			t.Fatalf("listServices(%d): %v", i, err)
		}
		for _, service := range services {
			if service.Name != serviceName {
				continue
			}
			if service.Spec == nil || serviceVolumeName(service.Spec) != volumeName {
				t.Fatalf("iteration %d: service %q lost its volume reference", i, serviceName)
			}
			state, err := store.desiredStateForAgent(ctx, service.AllocatedAgentID)
			if err != nil {
				t.Fatalf("desiredStateForAgent(%d): %v", i, err)
			}
			for _, desired := range state.GetServices() {
				if desired.GetServiceId() == service.ID && desired.GetVolumeId() == "" {
					t.Fatalf("iteration %d: desired state for %q has empty volume id", i, serviceName)
				}
			}
		}
	}
}
