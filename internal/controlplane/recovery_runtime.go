package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	agentv1 "ebof-wg-mesh/api/proto/agentv1"
	"ebof-wg-mesh/internal/config"
	"ebof-wg-mesh/internal/reconciliation"
	"ebof-wg-mesh/internal/recovery"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

func runtimeAuthority(cfg config.ControlPlaneConfig) (reconciliation.Authority, error) {
	if cfg.AuthorityFile == "" {
		if cfg.Profile.IsProduction() {
			return reconciliation.Authority{}, fmt.Errorf("production control plane requires --authority-file from host administration")
		}
		return reconciliation.Authority{}, nil
	}
	return reconciliation.ReadAuthority(cfg.AuthorityFile)
}

// Explicit allowlist: adding an RPC cannot silently enable recovery mutations.
func recoveryReadMethod(method string) bool {
	service, name, _ := strings.Cut(strings.TrimPrefix(method, "/"), "/")
	if service != "platform.v1.PlatformService" && service != "platform.v1.OpsService" {
		return false
	}
	switch name {
	case "ListProjects", "GetProject", "PreviewProjectDeletion", "ListEnvironments", "GetEnvironment", "PreviewEnvironmentDeletion", "GetService", "ListServices", "PreviewVolumeDeletion", "ListVolumes", "GetDomainBinding", "ListDomainBindings", "GetServiceStatus", "ListServiceLogs", "ListServiceDeployments", "ListServiceArtifacts", "ListAgents", "ListBuildAttempts", "ListFleet", "ListBuilders", "GetBuildScheduler":
		return true
	}
	return false
}

func recoveryUnary(paused bool) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, request any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		if paused && !recoveryReadMethod(info.FullMethod) {
			return nil, status.Error(codes.Unavailable, "recovery pauses mutations and certificate issuance")
		}
		return next(ctx, request)
	}
}

func recoveryStreams(paused bool) grpc.StreamServerInterceptor {
	return func(server any, stream grpc.ServerStream, info *grpc.StreamServerInfo, next grpc.StreamHandler) error {
		if paused && info.FullMethod != "/agent.v1.AgentControl/Sync" && !recoveryReadMethod(info.FullMethod) {
			return status.Error(codes.Unavailable, "recovery pauses workers")
		}
		return next(server, stream)
	}
}

func recoveryHTTP(paused bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if paused && !recoveryReadMethod(r.URL.Path) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"code": "unavailable", "message": "recovery pauses mutations and webhooks"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func withRecoveryAuthority(a reconciliation.Authority, inventoryDir string) agentServiceOption {
	return func(s *agentService) { s.recoveryAuthority, s.recoveryInventoryDir = a, inventoryDir }
}

// Inventory is persisted outside the restored SQL database. Authenticated agent
// identity has already been checked by Sync before this function runs.
func (s *agentService) collectRecoveryInventory(stream agentv1.AgentControl_SyncServer, hello *agentv1.AgentHello) error {
	if err := os.MkdirAll(s.recoveryInventoryDir, 0700); err != nil {
		return err
	}
	write := func(suffix string, b []byte) error {
		file, err := os.CreateTemp(s.recoveryInventoryDir, ".inventory-")
		if err != nil {
			return err
		}
		defer os.Remove(file.Name())
		if _, err := file.Write(b); err != nil {
			file.Close()
			return err
		}
		if err := file.Sync(); err != nil {
			file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		return os.Rename(file.Name(), filepath.Join(s.recoveryInventoryDir, hello.GetAgentId()+suffix+".json"))
	}
	if strings.ContainsAny(hello.GetAgentId(), "/\\") || hello.GetAgentId() == "." || hello.GetAgentId() == ".." {
		return status.Error(codes.InvalidArgument, "invalid inventory agent identity")
	}
	var inventory recovery.FleetHost
	if err := json.Unmarshal(hello.GetRecoveryInventory(), &inventory); err != nil {
		return status.Error(codes.InvalidArgument, "complete nonsecret recovery inventory required")
	}
	if inventory.ID != hello.GetAgentId() || inventory.Generation != s.recoveryAuthority.Generation {
		return status.Error(codes.PermissionDenied, "inventory differs from authenticated host admission")
	}
	if err := write("-fleet", hello.GetRecoveryInventory()); err != nil {
		return err
	}
	helloJSON, err := protojson.Marshal(hello)
	if err != nil {
		return err
	}
	if err := write("", helloJSON); err != nil {
		return err
	}
	for {
		message, err := stream.Recv()
		if err != nil {
			return err
		}
		if report := message.GetStatusReport(); report != nil {
			if report.GetAgentId() != hello.GetAgentId() || report.GetSessionId() != hello.GetSessionId() || report.GetInstallationId() != s.recoveryAuthority.InstallationID || report.GetRecoveryGeneration() != s.recoveryAuthority.Generation {
				return status.Error(codes.PermissionDenied, "inventory does not match admitted authority")
			}
			data, err := protojson.Marshal(report)
			if err != nil {
				return err
			}
			if err := write("-status", data); err != nil {
				return err
			}
		}
	}
}
