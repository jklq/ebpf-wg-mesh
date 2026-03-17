package integration_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	platformv1 "ebof-wg-mesh/api/proto/platformv1"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

const runEnv = "RUN_TESTBED_INTEGRATION"

type testbedHarness struct {
	repoRoot      string
	composeFile   string
	composeEnv    []string
	composeProj   string
	agentServices []string
	publicAPIURL  string
	publicAPIHost string
	signingKey    *rsa.PrivateKey
	platform      platformv1.PlatformServiceClient
	demoToken     string
	outsiderToken string
}

type testbedTopology struct {
	agentCount int
}

func TestTestbedIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testbed integration in short mode")
	}
	if os.Getenv(runEnv) != "1" {
		t.Skipf("set %s=1 to run Docker-backed testbed integration", runEnv)
	}

	h := startTestbedHarness(t)
	demoCtx := h.authContext(h.demoToken)
	outsiderCtx := h.authContext(h.outsiderToken)

	t.Run("Authentication", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := h.platform.ListProjects(ctx, &emptypb.Empty{})
		requireCode(t, err, codes.Unauthenticated)

		_, err = h.platform.ListProjects(h.authContext(h.mustSignToken(t, "demo-user", "demo@not-example.test", []string{"platform"})), &emptypb.Empty{})
		requireCode(t, err, codes.PermissionDenied)

		_, err = h.platform.ListProjects(h.authContext(h.mustSignToken(t, "demo-user", "demo@example.com", []string{"wrong-audience"})), &emptypb.Empty{})
		requireCode(t, err, codes.Unauthenticated)
	})

	t.Run("BootstrapState", func(t *testing.T) {
		t.Log("Listing bootstrap projects for demo user")
		projects, err := h.platform.ListProjects(demoCtx, &emptypb.Empty{})
		if err != nil {
			t.Fatalf("ListProjects(demo): %v", err)
		}
		t.Logf("Demo user projects: %v", projects.GetProjects())
		if len(projects.GetProjects()) != 1 || projects.GetProjects()[0].GetName() != "demo" {
			t.Fatalf("unexpected bootstrap projects: %+v", projects.GetProjects())
		}

		t.Log("Listing bootstrap projects for outsider user")
		outsiderProjects, err := h.platform.ListProjects(outsiderCtx, &emptypb.Empty{})
		if err != nil {
			t.Fatalf("ListProjects(outsider): %v", err)
		}
		t.Logf("Outsider user projects: %v", outsiderProjects.GetProjects())
		if len(outsiderProjects.GetProjects()) != 0 {
			t.Fatalf("expected outsider to have no projects, got %+v", outsiderProjects.GetProjects())
		}

		t.Log("Listing agents")
		agents, err := h.platform.ListAgents(demoCtx, &emptypb.Empty{})
		if err != nil {
			t.Fatalf("ListAgents: %v", err)
		}
		t.Logf("Found %d agents", len(agents.GetAgents()))
		if len(agents.GetAgents()) != 2 {
			t.Fatalf("expected 2 agents, got %d", len(agents.GetAgents()))
		}
		for _, agent := range agents.GetAgents() {
			t.Logf("Agent %s: healthy=%v", agent.GetId(), agent.GetHealthy())
			if !agent.GetHealthy() {
				t.Fatalf("expected agent %s to be healthy", agent.GetId())
			}
		}
	})

	t.Run("ProjectVolumeAndServiceLifecycle", func(t *testing.T) {
		projectName := "itest-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
		t.Logf("Creating project %s", projectName)
		project, err := h.platform.CreateProject(demoCtx, &platformv1.CreateProjectRequest{Name: projectName})
		if err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		t.Logf("Created project: %s", project.GetId())

		t.Logf("Getting project %s", project.GetId())
		gotProject, err := h.platform.GetProject(demoCtx, &platformv1.GetProjectRequest{ProjectId: project.GetId()})
		if err != nil {
			t.Fatalf("GetProject: %v", err)
		}
		if gotProject.GetId() != project.GetId() {
			t.Fatalf("expected project %s, got %s", project.GetId(), gotProject.GetId())
		}

		t.Logf("Verifying outsider cannot access project %s", project.GetId())
		_, err = h.platform.GetProject(outsiderCtx, &platformv1.GetProjectRequest{ProjectId: project.GetId()})
		requireCode(t, err, codes.NotFound)

		t.Logf("Creating volume for project %s", project.GetId())
		volume, err := h.platform.CreateVolume(demoCtx, &platformv1.CreateVolumeRequest{
			ProjectId: project.GetId(),
			Name:      "data",
			SizeBytes: 64 << 20,
		})
		if err != nil {
			t.Fatalf("CreateVolume: %v", err)
		}
		t.Logf("Created volume: %s bound to agent %s", volume.GetId(), volume.GetBoundAgentId())

		t.Logf("Listing volumes for project %s", project.GetId())
		volumes, err := h.platform.ListVolumes(demoCtx, &platformv1.ListVolumesRequest{ProjectId: project.GetId()})
		if err != nil {
			t.Fatalf("ListVolumes: %v", err)
		}
		if len(volumes.GetVolumes()) != 1 || volumes.GetVolumes()[0].GetId() != volume.GetId() {
			t.Fatalf("unexpected volumes: %+v", volumes.GetVolumes())
		}

		t.Logf("Waiting for volume directory to materialize on agent %s", volume.GetBoundAgentId())
		h.waitFor(t, 90*time.Second, "volume directory materialized", func() error {
			return h.composeExec(volume.GetBoundAgentId(), fmt.Sprintf("test -d /var/lib/platform-agent/volumes/%s", shellEscape(volume.GetId())))
		})

		t.Logf("Creating service for project %s", project.GetId())
		service, err := h.platform.CreateService(demoCtx, &platformv1.CreateServiceRequest{
			ProjectId: project.GetId(),
			Name:      "web",
			Spec: &platformv1.ServiceSpec{
				Image:         "docker.io/library/busybox:1.36",
				Command:       []string{"sh", "-c"},
				Args:          []string{"mkdir -p /www && echo ok >/www/index.html && exec httpd -f -p 8080 -h /www"},
				ContainerPort: 8080,
				VolumeName:    "data",
				HealthCheck: &platformv1.HealthCheck{
					Type:           platformv1.HealthCheck_TYPE_HTTP,
					Path:           "/",
					TimeoutSeconds: 2,
				},
			},
			Domains: []string{"web.demo.example.com"},
		})
		if err != nil {
			t.Fatalf("CreateService: %v", err)
		}
		t.Logf("Created service: %s on agent %s", service.GetId(), service.GetAllocatedAgentId())
		if service.GetAllocatedAgentId() != volume.GetBoundAgentId() {
			t.Fatalf("expected service agent %s to match volume agent %s", service.GetAllocatedAgentId(), volume.GetBoundAgentId())
		}

		t.Logf("Waiting for service %s to become healthy", service.GetId())
		statusResp := h.waitForServicePhase(t, demoCtx, project.GetId(), service.GetId(), 3*time.Minute, func(st *platformv1.ServiceStatus) error {
			t.Logf("Service status: phase=%s healthy=%v revision=%d endpoint=%s",
				st.GetAllocation().GetPhase(), st.GetAllocation().GetHealthy(),
				st.GetAllocation().GetAppliedRevision(), st.GetAllocation().GetEndpointAddr())
			if !hasString([]string{"Starting", "Running", "Healthy"}, st.GetAllocation().GetPhase()) {
				return fmt.Errorf("phase=%s healthy=%v", st.GetAllocation().GetPhase(), st.GetAllocation().GetHealthy())
			}
			if st.GetAllocation().GetEndpointAddr() == "" {
				return fmt.Errorf("endpoint address is empty")
			}
			if st.GetAllocation().GetAppliedRevision() != 1 {
				return fmt.Errorf("applied revision=%d", st.GetAllocation().GetAppliedRevision())
			}
			return nil
		})
		t.Logf("Service %s is healthy: endpoint=%s", service.GetId(), statusResp.GetAllocation().GetEndpointAddr())

		t.Logf("Verifying container present in agent %s containerd", service.GetAllocatedAgentId())
		h.waitFor(t, 90*time.Second, "container present in agent containerd", func() error {
			return h.composeExec(service.GetAllocatedAgentId(), fmt.Sprintf("ctr -n default container info platform-%s >/dev/null", shellEscape(statusResp.GetAllocation().GetAllocationId())))
		})

		t.Logf("Updating service %s to revision 2", service.GetId())
		updated, err := h.platform.UpdateService(demoCtx, &platformv1.UpdateServiceRequest{
			ProjectId: project.GetId(),
			ServiceId: service.GetId(),
			Spec: &platformv1.ServiceSpec{
				Image:         "docker.io/library/busybox:1.36",
				Command:       []string{"sh", "-c"},
				Args:          []string{"mkdir -p /www && echo updated >/www/index.html && exec httpd -f -p 8080 -h /www"},
				ContainerPort: 8080,
				VolumeName:    "data",
				HealthCheck: &platformv1.HealthCheck{
					Type:           platformv1.HealthCheck_TYPE_HTTP,
					Path:           "/",
					TimeoutSeconds: 2,
				},
			},
			Domains: []string{"web-updated.demo.example.com"},
		})
		if err != nil {
			t.Fatalf("UpdateService: %v", err)
		}
		if updated.GetCurrentRevision() != 2 {
			t.Fatalf("expected revision 2, got %d", updated.GetCurrentRevision())
		}

		t.Logf("Waiting for service %s to reach revision 2", service.GetId())
		h.waitForServicePhase(t, demoCtx, project.GetId(), service.GetId(), 3*time.Minute, func(st *platformv1.ServiceStatus) error {
			t.Logf("Service status: desired_rev=%d applied_rev=%d phase=%s healthy=%v",
				st.GetAllocation().GetDesiredRevision(), st.GetAllocation().GetAppliedRevision(),
				st.GetAllocation().GetPhase(), st.GetAllocation().GetHealthy())
			if !hasString([]string{"Starting", "Running", "Healthy"}, st.GetAllocation().GetPhase()) {
				return fmt.Errorf("phase=%s", st.GetAllocation().GetPhase())
			}
			if st.GetAllocation().GetDesiredRevision() != 2 {
				return fmt.Errorf("desired revision=%d", st.GetAllocation().GetDesiredRevision())
			}
			if st.GetAllocation().GetAppliedRevision() != 2 {
				return fmt.Errorf("applied revision=%d", st.GetAllocation().GetAppliedRevision())
			}
			return nil
		})

		t.Logf("Adding domain extra.demo.example.com to service %s", service.GetId())
		withExtraDomain, err := h.platform.UpsertDomain(demoCtx, &platformv1.UpsertDomainRequest{
			ProjectId: project.GetId(),
			ServiceId: service.GetId(),
			Domain:    "extra.demo.example.com",
		})
		if err != nil {
			t.Fatalf("UpsertDomain: %v", err)
		}
		t.Logf("Service domains: %v", withExtraDomain.GetDomains())
		if !hasString(withExtraDomain.GetDomains(), "extra.demo.example.com") {
			t.Fatalf("expected extra domain in %+v", withExtraDomain.GetDomains())
		}

		t.Logf("Deleting domain extra.demo.example.com from service %s", service.GetId())
		if _, err := h.platform.DeleteDomain(demoCtx, &platformv1.DeleteDomainRequest{
			ProjectId: project.GetId(),
			Domain:    "extra.demo.example.com",
		}); err != nil {
			t.Fatalf("DeleteDomain: %v", err)
		}

		afterDomainDelete, err := h.platform.GetService(demoCtx, &platformv1.GetServiceRequest{
			ProjectId: project.GetId(),
			ServiceId: service.GetId(),
		})
		if err != nil {
			t.Fatalf("GetService: %v", err)
		}
		t.Logf("Service domains after delete: %v", afterDomainDelete.GetDomains())
		if hasString(afterDomainDelete.GetDomains(), "extra.demo.example.com") {
			t.Fatalf("expected extra domain to be removed, got %+v", afterDomainDelete.GetDomains())
		}

		t.Logf("Deleting service %s", service.GetId())
		if _, err := h.platform.DeleteService(demoCtx, &platformv1.DeleteServiceRequest{
			ProjectId: project.GetId(),
			ServiceId: service.GetId(),
		}); err != nil {
			t.Fatalf("DeleteService: %v", err)
		}

		t.Logf("Waiting for service %s to be deleted from API", service.GetId())
		h.waitFor(t, 90*time.Second, "service deleted from api", func() error {
			_, err := h.platform.GetService(demoCtx, &platformv1.GetServiceRequest{
				ProjectId: project.GetId(),
				ServiceId: service.GetId(),
			})
			if status.Code(err) != codes.NotFound {
				return fmt.Errorf("expected not found, got %v", err)
			}
			return nil
		})

		t.Logf("Waiting for service %s to be removed from agent runtime", service.GetId())
		h.waitFor(t, 90*time.Second, "service removed from agent runtime", func() error {
			if err := h.composeExec(service.GetAllocatedAgentId(), fmt.Sprintf("! test -f /var/lib/platform-agent/desired/%s.json", shellEscape(statusResp.GetAllocation().GetAllocationId()))); err != nil {
				return err
			}
			return h.composeExec(service.GetAllocatedAgentId(), fmt.Sprintf("! ctr -n default container info platform-%s >/dev/null 2>&1", shellEscape(statusResp.GetAllocation().GetAllocationId())))
		})

		t.Logf("Deleting volume %s", volume.GetId())
		if _, err := h.platform.DeleteVolume(demoCtx, &platformv1.DeleteVolumeRequest{
			ProjectId: project.GetId(),
			VolumeId:  volume.GetId(),
		}); err != nil {
			t.Fatalf("DeleteVolume: %v", err)
		}

		t.Logf("Waiting for volume %s to be removed from agent runtime", volume.GetId())
		h.waitFor(t, 90*time.Second, "volume removed from agent runtime", func() error {
			return h.composeExec(volume.GetBoundAgentId(), fmt.Sprintf("! test -d /var/lib/platform-agent/volumes/%s", shellEscape(volume.GetId())))
		})
	})

	t.Run("SadPaths", func(t *testing.T) {
		t.Log("Testing resource limit enforcement")
		project, err := h.platform.CreateProject(demoCtx, &platformv1.CreateProjectRequest{Name: "sad-paths"})
		if err != nil {
			t.Fatalf("CreateProject: %v", err)
		}
		t.Logf("Created project %s for sad path tests", project.GetId())

		t.Log("Testing too much CPU/memory rejection")
		_, err = h.platform.CreateService(demoCtx, &platformv1.CreateServiceRequest{
			ProjectId: project.GetId(),
			Name:      "too-big",
			Spec: &platformv1.ServiceSpec{
				Image:           "docker.io/library/busybox:1.36",
				Command:         []string{"sh", "-c"},
				Args:            []string{"sleep 3600"},
				CpuMillis:       10000,
				MemoryMebibytes: 16384,
			},
		})
		requireCode(t, err, codes.FailedPrecondition)

		t.Log("Testing missing volume rejection during service creation")
		_, err = h.platform.CreateService(demoCtx, &platformv1.CreateServiceRequest{
			ProjectId: project.GetId(),
			Name:      "missing-volume",
			Spec: &platformv1.ServiceSpec{
				Image:      "docker.io/library/busybox:1.36",
				Command:    []string{"sh", "-c"},
				Args:       []string{"sleep 3600"},
				VolumeName: "does-not-exist",
			},
		})
		requireCode(t, err, codes.FailedPrecondition)

		t.Log("Testing bad image pull")
		badService, err := h.platform.CreateService(demoCtx, &platformv1.CreateServiceRequest{
			ProjectId: project.GetId(),
			Name:      "bad-image",
			Spec: &platformv1.ServiceSpec{
				Image:   "docker.io/library/busybox:not-a-real-tag-for-integration-suite",
				Command: []string{"sh", "-c"},
				Args:    []string{"sleep 3600"},
			},
		})
		if err != nil {
			t.Fatalf("CreateService(bad-image): %v", err)
		}
		t.Logf("Created service %s with bad image, waiting for error status", badService.GetId())

		h.waitForServicePhase(t, demoCtx, project.GetId(), badService.GetId(), 3*time.Minute, func(st *platformv1.ServiceStatus) error {
			t.Logf("Bad image service status: phase=%s healthy=%v message=%s",
				st.GetAllocation().GetPhase(), st.GetAllocation().GetHealthy(), st.GetAllocation().GetMessage())
			if st.GetAllocation().GetPhase() != "Error" {
				return fmt.Errorf("phase=%s", st.GetAllocation().GetPhase())
			}
			if st.GetAllocation().GetMessage() == "" {
				return fmt.Errorf("expected non-empty error message")
			}
			if st.GetAllocation().GetHealthy() {
				return fmt.Errorf("expected unhealthy allocation")
			}
			return nil
		})

		t.Log("Testing update of non-existent service returns NotFound")
		_, err = h.platform.UpdateService(demoCtx, &platformv1.UpdateServiceRequest{
			ProjectId: project.GetId(),
			ServiceId: "does-not-exist",
			Spec: &platformv1.ServiceSpec{
				Image:   "docker.io/library/busybox:1.36",
				Command: []string{"sh", "-c"},
				Args:    []string{"sleep 3600"},
			},
		})
		requireCode(t, err, codes.NotFound)

		t.Log("Testing upsert domain for non-existent service returns NotFound")
		_, err = h.platform.UpsertDomain(demoCtx, &platformv1.UpsertDomainRequest{
			ProjectId: project.GetId(),
			ServiceId: "does-not-exist",
			Domain:    "missing.demo.example.com",
		})
		requireCode(t, err, codes.NotFound)

		t.Log("Testing delete of non-existent domain returns NotFound")
		_, err = h.platform.DeleteDomain(demoCtx, &platformv1.DeleteDomainRequest{
			ProjectId: project.GetId(),
			Domain:    "missing.demo.example.com",
		})
		requireCode(t, err, codes.NotFound)

		t.Log("Creating a volume-backed service to verify in-use volume deletion is blocked")
		attachedVolume, err := h.platform.CreateVolume(demoCtx, &platformv1.CreateVolumeRequest{
			ProjectId: project.GetId(),
			Name:      "attached-data",
			SizeBytes: 32 << 20,
		})
		if err != nil {
			t.Fatalf("CreateVolume(attached-data): %v", err)
		}
		attachedService, err := h.platform.CreateService(demoCtx, &platformv1.CreateServiceRequest{
			ProjectId: project.GetId(),
			Name:      "attached-service",
			Spec: &platformv1.ServiceSpec{
				Image:      "docker.io/library/busybox:1.36",
				Command:    []string{"sh", "-c"},
				Args:       []string{"sleep 3600"},
				VolumeName: "attached-data",
			},
		})
		if err != nil {
			t.Fatalf("CreateService(attached-service): %v", err)
		}

		t.Log("Testing delete of in-use volume returns FailedPrecondition")
		_, err = h.platform.DeleteVolume(demoCtx, &platformv1.DeleteVolumeRequest{
			ProjectId: project.GetId(),
			VolumeId:  attachedVolume.GetId(),
		})
		requireCode(t, err, codes.FailedPrecondition)

		t.Log("Testing non-existent service returns NotFound")
		_, err = h.platform.GetService(demoCtx, &platformv1.GetServiceRequest{
			ProjectId: project.GetId(),
			ServiceId: "does-not-exist",
		})
		requireCode(t, err, codes.NotFound)

		t.Log("Testing non-existent volume returns NotFound")
		_, err = h.platform.DeleteVolume(demoCtx, &platformv1.DeleteVolumeRequest{
			ProjectId: project.GetId(),
			VolumeId:  "missing-volume",
		})
		requireCode(t, err, codes.NotFound)

		t.Logf("Cleaning up attached service %s", attachedService.GetId())
		if _, err := h.platform.DeleteService(demoCtx, &platformv1.DeleteServiceRequest{
			ProjectId: project.GetId(),
			ServiceId: attachedService.GetId(),
		}); err != nil {
			t.Fatalf("DeleteService(attached-service): %v", err)
		}

		t.Logf("Cleaning up attached volume %s", attachedVolume.GetId())
		if _, err := h.platform.DeleteVolume(demoCtx, &platformv1.DeleteVolumeRequest{
			ProjectId: project.GetId(),
			VolumeId:  attachedVolume.GetId(),
		}); err != nil {
			t.Fatalf("DeleteVolume(attached-data): %v", err)
		}

		t.Logf("Cleaning up bad-image service %s", badService.GetId())
		if _, err := h.platform.DeleteService(demoCtx, &platformv1.DeleteServiceRequest{
			ProjectId: project.GetId(),
			ServiceId: badService.GetId(),
		}); err != nil {
			t.Fatalf("DeleteService(bad-image): %v", err)
		}
	})
}

func TestTestbedTopologyMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping testbed integration in short mode")
	}
	if os.Getenv(runEnv) != "1" {
		t.Skipf("set %s=1 to run Docker-backed testbed integration", runEnv)
	}

	for _, agentCount := range []int{1, 2, 3} {
		agentCount := agentCount
		t.Run(fmt.Sprintf("%d-agents", agentCount), func(t *testing.T) {
			h := startTestbedHarnessForTopology(t, testbedTopology{agentCount: agentCount})
			demoCtx := h.authContext(h.demoToken)

			resp, err := h.platform.ListAgents(demoCtx, &emptypb.Empty{})
			if err != nil {
				t.Fatalf("ListAgents: %v", err)
			}
			if got := len(resp.GetAgents()); got != agentCount {
				t.Fatalf("expected %d agents, got %d", agentCount, got)
			}

			for _, service := range h.agentServices {
				expectedPeers := agentCount - 1
				h.waitFor(t, 90*time.Second, fmt.Sprintf("%s wireguard peers", service), func() error {
					count, err := h.wireGuardPeerCount(service)
					if err != nil {
						return err
					}
					if count != expectedPeers {
						return fmt.Errorf("expected %d peers, got %d", expectedPeers, count)
					}
					return nil
				})
			}
		})
	}
}

func startTestbedHarness(t *testing.T) *testbedHarness {
	return startTestbedHarnessForTopology(t, testbedTopology{agentCount: 2})
}

func startTestbedHarnessForTopology(t *testing.T, topology testbedTopology) *testbedHarness {
	t.Helper()
	if topology.agentCount <= 0 {
		t.Fatalf("agentCount must be greater than 0, got %d", topology.agentCount)
	}

	repoRoot := findRepoRoot(t)

	t.Log("Checking docker availability")
	assertCommandAvailable(t, "docker", "version")
	assertCommandAvailable(t, "docker", "compose", "version")

	signer, jwksPath := writeRuntimeConfig(t)
	caddyHTTPPort := freePort(t)
	caddyHTTPSPort := freePort(t)
	composeProject := fmt.Sprintf("testbed-%d", time.Now().UnixNano())
	composeFile := writeTestbedComposeFile(t, repoRoot, jwksPath, caddyHTTPPort, caddyHTTPSPort, topology.agentCount)

	t.Logf("Testbed config: project=%s agents=%d httpPort=%d httpsPort=%d",
		composeProject, topology.agentCount, caddyHTTPPort, caddyHTTPSPort)

	composeEnv := os.Environ()

	t.Cleanup(func() {
		if t.Failed() {
			t.Log("Fetching docker compose logs on failure...")
			if out, err := runCommandOutput(repoRoot, composeEnv, 30*time.Second, "docker", "compose", "-p", composeProject, "-f", composeFile, "logs", "--no-color"); err == nil && strings.TrimSpace(out) != "" {
				t.Logf("docker compose logs:\n%s", out)
			}
		}
		t.Logf("Tearing down testbed: %s", composeProject)
		_ = tryCommand(repoRoot, composeEnv, 2*time.Minute, "docker", "compose", "-p", composeProject, "-f", composeFile, "down", "-v", "--remove-orphans")
	})

	t.Log("Building and starting testbed containers (this takes several minutes)...")
	runCommand(t, repoRoot, composeEnv, 15*time.Minute, "docker", "compose", "--progress", "plain", "-p", composeProject, "-f", composeFile, "up", "-d", "--build")

	t.Log("Listing running testbed containers:")
	runCommand(t, repoRoot, composeEnv, 30*time.Second, "docker", "compose", "-p", composeProject, "-f", composeFile, "ps")

	h := &testbedHarness{
		repoRoot:      repoRoot,
		composeFile:   composeFile,
		composeEnv:    composeEnv,
		composeProj:   composeProject,
		agentServices: agentServiceNames(topology.agentCount),
		publicAPIURL:  fmt.Sprintf("http://127.0.0.1:%d", caddyHTTPPort),
		publicAPIHost: "platform.local",
		signingKey:    signer,
	}
	h.demoToken = h.mustSignToken(t, "demo-user", "demo@example.com", []string{"platform"})
	h.outsiderToken = h.mustSignToken(t, "outsider-user", "outsider@example.com", []string{"platform"})
	h.platform = newHTTPPlatformClient(h.publicAPIURL, h.publicAPIHost)

	h.waitFor(t, 4*time.Minute, "public http api ready", func() error {
		t.Log("Waiting for public HTTP API to be ready...")
		_, err := h.platform.ListProjects(h.authContext(h.demoToken), &emptypb.Empty{})
		return err
	})
	t.Log("Public HTTP API is ready")

	h.waitFor(t, 4*time.Minute, "both agents healthy", func() error {
		t.Log("Waiting for agents to become healthy...")
		resp, err := h.platform.ListAgents(h.authContext(h.demoToken), &emptypb.Empty{})
		if err != nil {
			return err
		}
		if len(resp.GetAgents()) != topology.agentCount {
			return fmt.Errorf("expected %d agents, got %d", topology.agentCount, len(resp.GetAgents()))
		}
		for _, agent := range resp.GetAgents() {
			t.Logf("Agent %s: healthy=%v", agent.GetId(), agent.GetHealthy())
			if !agent.GetHealthy() {
				return fmt.Errorf("agent %s not healthy yet", agent.GetId())
			}
		}
		return nil
	})
	t.Log("Requested agents are healthy")

	return h
}

func writeTestbedComposeFile(t *testing.T, repoRoot, jwksPath string, caddyHTTPPort, caddyHTTPSPort, agentCount int) string {
	t.Helper()

	composePath := filepath.Join(t.TempDir(), "docker-compose.yml")
	underlaySubnet, serviceIPs := uniqueUnderlayNetwork(agentCount)

	var b strings.Builder
	b.WriteString("services:\n")
	b.WriteString(`  cockroach:
    image: cockroachdb/cockroach:v26.1.0
    command: ["start-single-node", "--insecure", "--advertise-addr=localhost", "--listen-addr=127.0.0.1:26257", "--sql-addr=0.0.0.0:26258", "--http-addr=0.0.0.0:8081"]
    volumes:
      - cockroach-data:/cockroach/cockroach-data
    networks:
      underlay:
        ipv6_address: "` + serviceIPs["cockroach"] + `"
    healthcheck:
      test: ["CMD-SHELL", "cockroach sql --insecure --host=127.0.0.1:26258 --execute='select 1' >/dev/null 2>&1"]
      interval: 5s
      timeout: 3s
      retries: 40

  controlplane:
    build:
      context: "` + repoRoot + `"
      dockerfile: "` + filepath.Join(repoRoot, "testbed", "Dockerfile") + `"
    image: ebof-wg-mesh-dev:latest
    command: ["-lc", "/usr/local/bin/controlplane -bootstrap-user demo-user:demo@example.com:demo"]
    environment:
      CONTROLPLANE_PUBLIC_LISTEN: 0.0.0.0:8080
      CONTROLPLANE_INTERNAL_SERVER_NAMES: controlplane,controlplane-internal,localhost
      CONTROLPLANE_AGENT_BOOTSTRAP_TOKENS: dev-bootstrap-token
      CONTROLPLANE_DB_URL: postgresql://root@cockroach:26258/defaultdb?sslmode=disable&application_name=controlplane
      CONTROLPLANE_STATE_DIR: /var/lib/platform
      CONTROLPLANE_OIDC_ISSUER: https://platform.local
      CONTROLPLANE_OIDC_AUDIENCE: platform
      CONTROLPLANE_OIDC_JWKS_URL: /config/jwks.json
      CONTROLPLANE_OIDC_ALLOWED_EMAIL_DOMAIN: example.com
      CONTROLPLANE_INGRESS_ADMIN_URL: http://caddy:2019/load
      CONTROLPLANE_INGRESS_PUBLIC_ADDR: platform.local
      CONTROLPLANE_INGRESS_CONTROLPLANE_UPSTREAM: controlplane:8080
    volumes:
      - "` + jwksPath + `:/config/jwks.json:ro"
      - "` + filepath.Join(repoRoot, "testbed", "scripts") + `:/testbed/scripts:ro"
      - controlplane-data:/var/lib/platform
    networks:
      underlay:
        ipv6_address: "` + serviceIPs["controlplane"] + `"
    depends_on:
      caddy:
        condition: service_started
      cockroach:
        condition: service_healthy
    healthcheck:
      test: ["CMD-SHELL", "nc -z 127.0.0.1 8080 && nc -z 127.0.0.1 9443"]
      interval: 5s
      timeout: 3s
      retries: 40

  caddy:
    image: caddy:2.10.2
    command: ["caddy", "run", "--config", "/etc/caddy/devstack.json"]
    volumes:
      - "` + filepath.Join(repoRoot, "testbed", "config", "caddy.json") + `:/etc/caddy/devstack.json:ro"
      - caddy-data:/data
      - caddy-config:/config
    ports:
      - "127.0.0.1:` + fmt.Sprintf("%d", caddyHTTPPort) + `:80"
      - "127.0.0.1:` + fmt.Sprintf("%d", caddyHTTPSPort) + `:443"
    networks:
      underlay:
        ipv6_address: "` + serviceIPs["caddy"] + `"
`)

	for i := 1; i <= agentCount; i++ {
		service := agentServiceName(i)
		b.WriteString(fmt.Sprintf(`
  %s:
    image: ebof-wg-mesh-dev:latest
    privileged: true
    hostname: %s
    command: ["-lc", "/testbed/scripts/agent-entrypoint.sh"]
    environment:
      AGENT_CONTROLPLANE_ADDRESS: controlplane:9443
      AGENT_CA_FILE: /controlplane-state/pki/ca.crt
      AGENT_SERVER_NAME: controlplane
      AGENT_BOOTSTRAP_TOKEN: dev-bootstrap-token
      AGENT_DATA_DIR: /var/lib/platform-agent
      CNI_NETWORK_NAME: mesh-cni
      CNI_SUBNET: fd00:200:0:%x::/64
      CNI_GATEWAY: fd00:200:0:%x::1
      MESH_ROUTE_CIDR: fd00:200::/48
    sysctls:
      net.ipv4.ip_forward: "1"
      net.ipv6.conf.all.forwarding: "1"
      net.ipv4.conf.all.rp_filter: "0"
      net.ipv4.conf.default.rp_filter: "0"
    volumes:
      - controlplane-data:/controlplane-state:ro
      - "%s:/testbed/scripts:ro"
      - %s-data:/var/lib/platform-agent
    networks:
      underlay:
        ipv6_address: "%s"
    healthcheck:
      test: ["CMD-SHELL", "ctr version >/dev/null 2>&1 && ip link show wg0 | grep -q UP"]
      interval: 5s
      timeout: 3s
      retries: 40
    depends_on:
      controlplane:
        condition: service_healthy
`, service, service, i, i, filepath.Join(repoRoot, "testbed", "scripts"), service, serviceIPs[service]))
	}

	b.WriteString(fmt.Sprintf(`
networks:
  underlay:
    driver: bridge
    enable_ipv6: true
    ipam:
      config:
        - subnet: "%s"

volumes:
  cockroach-data:
  controlplane-data:
  caddy-data:
  caddy-config:
`, underlaySubnet))
	for i := 1; i <= agentCount; i++ {
		b.WriteString(fmt.Sprintf("  %s-data:\n", agentServiceName(i)))
	}

	if err := os.WriteFile(composePath, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write compose file: %v", err)
	}
	return composePath
}

func uniqueUnderlayNetwork(agentCount int) (string, map[string]string) {
	// Pick a unique fd00:30:xxxx::/64 subnet to reduce collisions across parallel runs.
	segment := uint16(time.Now().UnixNano()%65535) + 1
	base := fmt.Sprintf("fd00:30:%x", segment)

	ips := map[string]string{
		"cockroach":    base + "::30",
		"controlplane": base + "::10",
		"caddy":        base + "::20",
	}
	for i := 1; i <= agentCount; i++ {
		ips[agentServiceName(i)] = fmt.Sprintf("%s::%x", base, 0x10+i)
	}
	return base + "::/64", ips
}

func agentServiceNames(agentCount int) []string {
	names := make([]string, 0, agentCount)
	for i := 1; i <= agentCount; i++ {
		names = append(names, agentServiceName(i))
	}
	return names
}

func agentServiceName(i int) string {
	return fmt.Sprintf("agent-node%d", i)
}

func (h *testbedHarness) authContext(token string) context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+token)
}

func (h *testbedHarness) waitForServicePhase(t *testing.T, ctx context.Context, projectID, serviceID string, timeout time.Duration, predicate func(*platformv1.ServiceStatus) error) *platformv1.ServiceStatus {
	t.Helper()
	var last *platformv1.ServiceStatus
	h.waitFor(t, timeout, fmt.Sprintf("service %s status", serviceID), func() error {
		resp, err := h.platform.GetServiceStatus(ctx, &platformv1.GetServiceStatusRequest{
			ProjectId: projectID,
			ServiceId: serviceID,
		})
		if err != nil {
			return err
		}
		last = resp
		return predicate(resp)
	})
	return last
}

func (h *testbedHarness) waitFor(t *testing.T, timeout time.Duration, description string, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := fn(); err == nil {
			return
		} else {
			lastErr = err
			t.Logf("Waiting for %s: %v", description, err)
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("timed out waiting for %s: %v", description, lastErr)
}

func (h *testbedHarness) mustSignToken(t *testing.T, subject, email string, audience []string) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub":   subject,
		"email": email,
		"iss":   "https://platform.local",
		"aud":   audience,
		"exp":   time.Now().Add(time.Hour).Unix(),
		"iat":   time.Now().Add(-time.Minute).Unix(),
	})
	token.Header["kid"] = "testbed-suite"
	raw, err := token.SignedString(h.signingKey)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return raw
}

func (h *testbedHarness) composeExec(service, script string) error {
	out, err := runCommandOutput(h.repoRoot, h.composeEnv, 30*time.Second, "docker", "compose", "-p", h.composeProj, "-f", h.composeFile, "exec", "-T", service, "sh", "-lc", script)
	if err != nil {
		return fmt.Errorf("compose exec %s: %w\n%s", service, err, out)
	}
	return nil
}

func (h *testbedHarness) wireGuardPeerCount(service string) (int, error) {
	out, err := runCommandOutput(h.repoRoot, h.composeEnv, 30*time.Second, "docker", "compose", "-p", h.composeProj, "-f", h.composeFile, "exec", "-T", service, "sh", "-lc", "wg show wg0 peers")
	if err != nil {
		return 0, fmt.Errorf("wg show %s: %w\n%s", service, err, out)
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count, nil
}

func writeRuntimeConfig(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	tmpDir := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	jwksDoc := map[string]any{
		"keys": []map[string]string{{
			"kid": "testbed-suite",
			"kty": "RSA",
			"n":   base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x00, 0x01}),
		}},
	}
	jwksBytes, err := json.MarshalIndent(jwksDoc, "", "  ")
	if err != nil {
		t.Fatalf("marshal JWKS: %v", err)
	}

	jwksPath := filepath.Join(tmpDir, "jwks.json")
	if err := os.WriteFile(jwksPath, jwksBytes, 0o644); err != nil {
		t.Fatalf("write jwks: %v", err)
	}

	return key, jwksPath
}

func assertCommandAvailable(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := append([]string{name}, args...)
	_, err := runCommandOutput("", nil, 20*time.Second, cmd...)
	if err != nil {
		t.Fatalf("%s unavailable: %v", name, err)
	}
}

func runCommand(t *testing.T, dir string, env []string, timeout time.Duration, args ...string) string {
	t.Helper()
	out, err := runCommandOutput(dir, env, timeout, args...)
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Logf("%s output:\n%s", strings.Join(args, " "), out)
	}
	return out
}

func tryCommand(dir string, env []string, timeout time.Duration, args ...string) error {
	_, err := runCommandOutput(dir, env, timeout, args...)
	return err
}

func runCommandOutput(dir string, env []string, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return string(out), ctx.Err()
	}
	return string(out), err
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate repository root from current working directory")
		}
		dir = parent
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func requireCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("expected gRPC code %s, got %s (err=%v)", want, got, err)
	}
}

func hasString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func shellEscape(s string) string {
	return strings.ReplaceAll(s, `'`, `'\''`)
}
