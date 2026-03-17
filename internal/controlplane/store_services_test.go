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
	_, err = store.createService(ctx, "user-1", projects[0].ID, "web", &platformv1.ServiceSpec{
		Image:           "busybox:1.36",
		CpuMillis:       100,
		MemoryMebibytes: 64,
		ContainerPort:   8080,
		VolumeName:      "data",
	}, "node-1", nil)
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
	if _, err := store.createService(ctx, "user-1", projects[0].ID, "web", &platformv1.ServiceSpec{
		Image:      "busybox:1.36",
		VolumeName: "data",
	}, "node-1", nil); err != nil {
		t.Fatalf("createService: %v", err)
	}

	err = store.deleteVolume(ctx, "user-1", projects[0].ID, volume.ID)
	if !errors.Is(err, errVolumeInUse) {
		t.Fatalf("expected errVolumeInUse, got %v", err)
	}
}

func TestConcurrentCreateServicePlacementIsAtomic(t *testing.T) {
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

	for _, id := range []string{"node-a", "node-b"} {
		hello := agentHello(id)
		hello.CpuMillisCapacity = 100
		hello.MemoryMebibytesCapacity = 128
		if _, err := store.upsertAgent(ctx, hello); err != nil {
			t.Fatalf("upsertAgent(%s): %v", id, err)
		}
	}

	start := make(chan struct{})
	type result struct {
		rec serviceRecord
		err error
	}
	results := make(chan result, 2)
	for _, name := range []string{"web-a", "web-b"} {
		name := name
		go func() {
			<-start
			rec, err := store.createScheduledService(ctx, "user-1", projects[0].ID, name, &platformv1.ServiceSpec{
				Image:           "busybox:1.36",
				CpuMillis:       100,
				MemoryMebibytes: 64,
				ContainerPort:   8080,
			}, nil)
			results <- result{rec: rec, err: err}
		}()
	}

	close(start)
	first := <-results
	second := <-results
	if first.err != nil {
		t.Fatalf("first createScheduledService: %v", first.err)
	}
	if second.err != nil {
		t.Fatalf("second createScheduledService: %v", second.err)
	}
	if first.rec.AllocatedAgentID == second.rec.AllocatedAgentID {
		t.Fatalf("expected placement to spread across agents, both services landed on %q", first.rec.AllocatedAgentID)
	}
}

func TestConcurrentUpdateServiceAdvancesUniqueRevisions(t *testing.T) {
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

	service, err := store.createService(ctx, "user-1", projects[0].ID, "web", &platformv1.ServiceSpec{
		Image:           "busybox:1.36",
		CpuMillis:       100,
		MemoryMebibytes: 64,
		ContainerPort:   8080,
	}, "node-1", []string{"web.example.com"})
	if err != nil {
		t.Fatalf("createService: %v", err)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	for i, port := range []int32{8081, 8082} {
		i := i
		port := port
		go func() {
			<-start
			_, err := store.updateService(ctx, "user-1", projects[0].ID, service.ID, &platformv1.ServiceSpec{
				Image:           "busybox:1.36",
				CpuMillis:       100,
				MemoryMebibytes: 64 + int64(i),
				ContainerPort:   port,
			}, []string{fmt.Sprintf("web-%d.example.com", i)})
			errs <- err
		}()
	}

	close(start)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("updateService: %v", err)
		}
	}

	current, err := store.serviceByID(ctx, "user-1", projects[0].ID, service.ID)
	if err != nil {
		t.Fatalf("serviceByID: %v", err)
	}
	if current.CurrentRevision != 3 {
		t.Fatalf("expected current revision 3, got %d", current.CurrentRevision)
	}
	var revisions int
	if revisions, err = store.countServiceRevisionsForTest(ctx, service.ID); err != nil {
		t.Fatalf("count revisions: %v", err)
	}
	if revisions != 3 {
		t.Fatalf("expected 3 stored revisions, got %d", revisions)
	}
}

func TestConcurrentDeleteVolumeAndCreateServiceStayConsistent(t *testing.T) {
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
			_, err := store.createScheduledService(ctx, "user-1", projects[0].ID, serviceName, &platformv1.ServiceSpec{
				Image:           "busybox:1.36",
				CpuMillis:       100,
				MemoryMebibytes: 64,
				ContainerPort:   8080,
				VolumeName:      volumeName,
			}, nil)
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
			if service.Spec == nil || service.Spec.GetVolumeName() != volumeName {
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
